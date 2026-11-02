package onlineupdate

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Source is the non-secret, host-persisted GitHub repository and GHCR image policy.
type Source struct {
	RepositoryURL string `json:"repositoryUrl"`
	Image         string `json:"image"`
}

type sourceContextKey struct{}

var ownerPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,99}$`)
var imageNamePattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)

// DefaultSource returns the migration default without shared mutable configuration.
func DefaultSource() Source {
	return Source{RepositoryURL: "https://github.com/" + Repository, Image: Image}
}

// normalizeSource validates fixed HTTPS/GHCR hosts and canonicalizes only optional .git and slash.
func normalizeSource(source Source) (Source, error) {
	parsed, err := url.Parse(source.RepositoryURL)
	if err != nil || len(source.RepositoryURL) > 256 || parsed.Scheme != "https" || parsed.Host != "github.com" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.ContainsAny(source.RepositoryURL, "%?#\\") {
		return Source{}, errors.New("仓库必须是 https://github.com/owner/repo，不支持端口、参数或凭据")
	}
	path := strings.TrimSuffix(parsed.Path, "/")
	path = strings.TrimSuffix(path, ".git")
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) != 2 || !ownerPattern.MatchString(parts[0]) || strings.Contains(parts[0], "--") || !repositoryPattern.MatchString(parts[1]) || strings.Contains(parts[1], "..") {
		return Source{}, errors.New("GitHub 仓库名称无效")
	}
	imageParts := strings.Split(source.Image, "/")
	if len(source.Image) > 255 || len(imageParts) != 3 || imageParts[0] != "ghcr.io" || !ownerPattern.MatchString(imageParts[1]) || imageParts[1] != strings.ToLower(imageParts[1]) || strings.Contains(imageParts[1], "--") || !imageNamePattern.MatchString(imageParts[2]) {
		return Source{}, errors.New("镜像必须是 ghcr.io/owner/name，不支持 tag 或 digest")
	}
	source.RepositoryURL = "https://github.com/" + parts[0] + "/" + parts[1]
	return source, nil
}

// repository extracts the owner/repo path only from an already validated source.
func (source Source) repository() string {
	return strings.TrimPrefix(source.RepositoryURL, "https://github.com/")
}

// sourceContext binds an immutable per-operation policy snapshot to existing host/source interfaces.
func sourceContext(ctx context.Context, source Source) context.Context {
	return context.WithValue(ctx, sourceContextKey{}, source)
}

// contextSource resolves the snapshot, preserving defaults for existing injected fakes and callers.
func contextSource(ctx context.Context) (Source, error) {
	source, ok := ctx.Value(sourceContextKey{}).(Source)
	if !ok {
		source = DefaultSource()
	}
	return normalizeSource(source)
}

// validateReleaseFor permits exactly the source's image, stable version, digest and platform.
func validateReleaseFor(release Release, source Source) error {
	normalized, err := normalizeSource(source)
	if err != nil {
		return err
	}
	if _, err = versionParts(release.Version); err != nil {
		return err
	}
	if release.Image != normalized.Image || release.Platform != Platform || !digestPattern.MatchString(release.Digest) {
		return errors.New("release policy rejected")
	}
	return nil
}

// Source returns a detached source configuration and does not perform any network operation.
func (service *Service) Source() Source {
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.record.Source
}

// SaveSource atomically persists policy changes before publishing them, rejecting active/recovery work.
func (service *Service) SaveSource(source Source) (Source, error) {
	normalized, err := normalizeSource(source)
	if err != nil {
		return Source{}, err
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.busy || service.broken || service.record.Status.State == "manual_recovery" || service.ports.State == "manual_recovery" {
		return Source{}, errors.New("操作执行中或需要人工恢复，不能修改更新来源")
	}
	candidate := service.record
	candidate.Source = normalized
	candidate.Status.Latest = nil
	candidate.Status.CanApply = false
	candidate.Status.State = "idle"
	candidate.Status.Phase = "source"
	candidate.Status.Message = "更新来源已保存，请手动检测正式版本"
	candidate.Status.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err = service.store.Save(candidate); err != nil {
		service.log("source persistence failed")
		return Source{}, errors.New("更新来源保存失败，原配置保持不变")
	}
	service.record = candidate
	service.log("update source changed")
	return normalized, nil
}
