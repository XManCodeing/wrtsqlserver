package onlineupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// GitHub reads the operation's validated repository and bounded release manifests.
type GitHub struct {
	Client *http.Client
	Token  func() (string, error)
}

// NewGitHub creates a client with strict cross-origin redirect and credential policy.
func NewGitHub() *GitHub {
	return &GitHub{Client: &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 45 * time.Second, CheckRedirect: releaseRedirect}, Token: readToken}
}

// readToken returns no credential when the optional file is absent; unsafe files still fail closed.
func readToken() (string, error) {
	data, err := privateRead(TokenPath, 4096)
	return optionalToken(data, err)
}

// optionalToken validates privateRead output; only absence permits anonymous public access.
// data contains private credential bytes; readErr is the secure reader's error. Returns a token or safe error.
func optionalToken(data []byte, readErr error) (string, error) {
	if errors.Is(readErr, os.ErrNotExist) {
		return "", nil
	}
	if readErr != nil {
		return "", errors.New("GitHub credential unavailable")
	}
	token := strings.TrimSpace(string(data))
	if token == "" || strings.ContainsAny(token, "\r\n\t ") {
		return "", errors.New("invalid GitHub credential")
	}
	return token, nil
}

// releaseRedirect permits only HTTPS release asset hosts and always strips credentials.
func releaseRedirect(request *http.Request, previous []*http.Request) error {
	request.Header.Del("Authorization")
	if len(previous) > 3 || request.URL.Scheme != "https" || request.URL.User != nil || request.URL.Port() != "" {
		return errors.New("release redirect rejected")
	}
	switch request.URL.Host {
	case "release-assets.githubusercontent.com", "objects.githubusercontent.com":
	default:
		return errors.New("release redirect host rejected")
	}
	if len(previous) == 0 {
		return errors.New("metadata redirect rejected")
	}
	source, err := contextSource(previous[0].Context())
	if err != nil || previous[0].URL.Scheme != "https" || previous[0].URL.Host != "api.github.com" || !assetAPIPath(previous[0].URL.Path, source.repository()) {
		return errors.New("metadata redirect rejected")
	}
	return nil
}

// assetAPIPath accepts only a positive canonical asset identifier in this operation's repository.
func assetAPIPath(path, repository string) bool {
	prefix := "/repos/" + repository + "/releases/assets/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	identifier := strings.TrimPrefix(path, prefix)
	value, err := strconv.ParseInt(identifier, 10, 64)
	return err == nil && value > 0 && strconv.FormatInt(value, 10) == identifier
}

// apiGet fetches a fixed API path with a token; errors never include headers or signed URLs.
func (github *GitHub) apiGet(ctx context.Context, path, accept string, limit int64) ([]byte, error) {
	source, sourceErr := contextSource(ctx)
	if sourceErr != nil {
		return nil, sourceErr
	}
	if path != "/repos/"+source.repository()+"/releases/latest" && !assetAPIPath(path, source.repository()) {
		return nil, errors.New("API path rejected")
	}
	token, err := github.Token()
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com"+path, nil)
	if err != nil {
		return nil, errors.New("cannot create GitHub request")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	request.Header.Set("Accept", accept)
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "relay-updater")
	client := *github.Client
	client.CheckRedirect = releaseRedirect
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("GitHub request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub returned HTTP %d; private releases require repository read permission", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("GitHub response exceeds limit or is unreadable")
	}
	return data, nil
}

// Latest authenticates metadata and the asset API, then validates tag/manifest consistency.
func (github *GitHub) Latest(ctx context.Context) (Release, error) {
	var result Release
	source, sourceErr := contextSource(ctx)
	if sourceErr != nil {
		return result, sourceErr
	}
	data, err := github.apiGet(ctx, "/repos/"+source.repository()+"/releases/latest", "application/vnd.github+json", 1024*1024)
	if err != nil {
		return result, err
	}
	var metadata struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
		Assets     []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
			Size int64  `json:"size"`
			URL  string `json:"url"`
		} `json:"assets"`
	}
	if err = json.Unmarshal(data, &metadata); err != nil || metadata.Draft || metadata.Prerelease || !strings.HasPrefix(metadata.Tag, "v") {
		return result, errors.New("not a stable release")
	}
	if _, err = versionParts(strings.TrimPrefix(metadata.Tag, "v")); err != nil {
		return result, err
	}
	assetPath := ""
	for _, asset := range metadata.Assets {
		if asset.Name != "relay-release.json" {
			continue
		}
		expected := "/repos/" + source.repository() + "/releases/assets/" + strconv.FormatInt(asset.ID, 10)
		parsed, parseErr := url.Parse(asset.URL)
		if assetPath != "" || asset.ID <= 0 || asset.Size <= 0 || asset.Size > 16*1024 || parseErr != nil || parsed.String() != "https://api.github.com"+expected {
			return result, errors.New("release asset rejected")
		}
		assetPath = expected
	}
	if assetPath == "" {
		return result, errors.New("release manifest missing")
	}
	data, err = github.apiGet(ctx, assetPath, "application/octet-stream", 16*1024)
	if err != nil {
		return result, err
	}
	if err = decodeStrict(strings.NewReader(string(data)), 16*1024, &result); err != nil {
		return result, err
	}
	if err = validateReleaseFor(result, source); err != nil {
		return result, err
	}
	if metadata.Tag != "v"+result.Version {
		return result, errors.New("release tag mismatch")
	}
	return result, nil
}
