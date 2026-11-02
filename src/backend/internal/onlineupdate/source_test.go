package onlineupdate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// alternativeSource is a valid non-default repository policy used only in tests.
func alternativeSource() Source {
	return Source{"https://github.com/OtherOwner/relay", "ghcr.io/otherowner/relay"}
}

// TestSourceValidation rejects URL authority, encoding and image reference escapes.
func TestSourceValidation(t *testing.T) {
	for _, repository := range []string{"http://github.com/a/b", "https://github.com:443/a/b", "https://u@github.com/a/b", "https://github.com/a/b?", "https://github.com/a/b#", "https://github.com/a/%2e%2e", "https://github.com/a/b%2fc", "https://github.com/a/b/c", "https://evil.example/a/b", "https://github.com/a/..", "https://github.com/a/b\\c"} {
		source := alternativeSource()
		source.RepositoryURL = repository
		if _, err := normalizeSource(source); err == nil {
			t.Errorf("accepted unsafe repository %s", repository)
		}
	}
	for _, image := range []string{"docker.io/a/b", "ghcr.io/a/b:latest", "ghcr.io/a/b@sha256:abc", "ghcr.io:443/a/b", "ghcr.io/A/b", "ghcr.io/a/../b", "ghcr.io/a/b/c", "ghcr.io/a/.b", "ghcr.io/a/b%2fc"} {
		source := alternativeSource()
		source.Image = image
		if _, err := normalizeSource(source); err == nil {
			t.Errorf("accepted unsafe image %s", image)
		}
	}
	source := alternativeSource()
	source.RepositoryURL += ".git/"
	normalized, err := normalizeSource(source)
	if err != nil || normalized != alternativeSource() {
		t.Fatal("normalization failed")
	}
}

// TestSourcePersistenceAndCache covers default migration, save, cache invalidation and restart.
func TestSourcePersistenceAndCache(t *testing.T) {
	store := &memoryStore{exists: true, record: Record{Status: Status{State: "succeeded", CanRollback: true}, PreviousVersion: "0.9.0"}}
	host := &fakeHost{current: "1.0.0", transaction: true, applied: true}
	service := newTestService(t, host, &fakeSource{release: testRelease()}, store)
	if service.Source() != DefaultSource() {
		t.Fatal("old state did not migrate")
	}
	if err := service.Check(); err != nil {
		t.Fatal(err)
	}
	service.Wait()
	if !service.Status().CanApply {
		t.Fatal("fixture has no cached release")
	}
	if _, err := service.SaveSource(alternativeSource()); err != nil {
		t.Fatal(err)
	}
	status := service.Status()
	if status.Latest != nil || status.CanApply || !status.CanRollback {
		t.Fatal("cache or rollback invalidation wrong")
	}
	saved, _ := store.Load()
	if saved.PreviousVersion != "0.9.0" {
		t.Fatal("rollback metadata lost")
	}
	restarted := newTestService(t, host, &fakeSource{}, store)
	if restarted.Source() != alternativeSource() {
		t.Fatal("source did not survive restart")
	}
}

// TestSourceSaveFailureDoesNotPublish verifies failed persistence leaves configuration and cache intact.
func TestSourceSaveFailureDoesNotPublish(t *testing.T) {
	store := &memoryStore{}
	service := newTestService(t, &fakeHost{current: "1.0.0"}, &fakeSource{release: testRelease()}, store)
	if err := service.Check(); err != nil {
		t.Fatal(err)
	}
	service.Wait()
	before := service.Status()
	store.fail = true
	if _, err := service.SaveSource(alternativeSource()); err == nil {
		t.Fatal("failed persistence accepted")
	}
	after := service.Status()
	if service.Source() != DefaultSource() || after.Latest == nil || *after.Latest != *before.Latest || after.UpdatedAt != before.UpdatedAt {
		t.Fatal("failed save became visible")
	}
}

// TestSourceSaveMutualExclusion refuses edits during checks, recovery and broken persistence.
func TestSourceSaveMutualExclusion(t *testing.T) {
	source := &fakeSource{release: testRelease(), gate: make(chan struct{})}
	service := newTestService(t, &fakeHost{current: "1.0.0"}, source, &memoryStore{})
	if err := service.Check(); err != nil {
		t.Fatal(err)
	}
	if _, err := service.SaveSource(alternativeSource()); err == nil {
		t.Error("edit during check accepted")
	}
	close(source.gate)
	service.Wait()
	service.mu.Lock()
	service.record.Status.State = "manual_recovery"
	service.mu.Unlock()
	if _, err := service.SaveSource(alternativeSource()); err == nil {
		t.Error("edit during recovery accepted")
	}
	service.mu.Lock()
	service.record.Status.State = "idle"
	service.broken = true
	service.mu.Unlock()
	if _, err := service.SaveSource(alternativeSource()); err == nil {
		t.Error("edit with broken store accepted")
	}
}

// TestSourceHTTPContract returns the source object itself, not the operation status wrapper.
func TestSourceHTTPContract(t *testing.T) {
	service := newTestService(t, &fakeHost{current: "1.0.0"}, &fakeSource{}, &memoryStore{})
	for _, method := range []string{"GET", "POST"} {
		body := ""
		expected := DefaultSource()
		if method == "POST" {
			data, _ := json.Marshal(alternativeSource())
			body = string(data)
			expected = alternativeSource()
		}
		response := httptest.NewRecorder()
		service.Handler().ServeHTTP(response, httptest.NewRequest(method, "/source", strings.NewReader(body)))
		var source Source
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &source) != nil || source != expected {
			t.Fatal("source HTTP contract failed")
		}
	}
}

// TestConfiguredGitHubRoutes verifies the same credential stays on api.github.com for the configured repo.
func TestConfiguredGitHubRoutes(t *testing.T) {
	source := alternativeSource()
	release := testRelease()
	release.Image = source.Image
	requests := 0
	github := &GitHub{Token: func() (string, error) { return "private-fixture", nil }, Client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.URL.Host != "api.github.com" || request.Header.Get("Authorization") != "Bearer private-fixture" {
			t.Error("credential destination invalid")
		}
		var body []byte
		switch request.URL.Path {
		case "/repos/OtherOwner/relay/releases/latest":
			body = []byte(`{"tag_name":"v1.2.0","assets":[{"id":5,"name":"relay-release.json","size":200,"url":"https://api.github.com/repos/OtherOwner/relay/releases/assets/5"}]}`)
		case "/repos/OtherOwner/relay/releases/assets/5":
			body, _ = json.Marshal(release)
		default:
			return nil, errors.New("unexpected repository route")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body))), Header: http.Header{}, Request: request}, nil
	})}}
	ctx := sourceContext(context.Background(), source)
	result, err := github.Latest(ctx)
	if err != nil || result.Image != source.Image || requests != 2 {
		t.Fatalf("configured release failed: %v", err)
	}
	if _, err = github.apiGet(ctx, "/repos/"+Repository+"/releases/latest", "application/json", 4096); err == nil {
		t.Fatal("cross-repository request accepted")
	}
}

// TestConfiguredRedirectStripsToken checks custom repository asset redirects and rejects other repos.
func TestConfiguredRedirectStripsToken(t *testing.T) {
	source := alternativeSource()
	origin, _ := http.NewRequestWithContext(sourceContext(context.Background(), source), "GET", "https://api.github.com/repos/OtherOwner/relay/releases/assets/5", nil)
	target, _ := http.NewRequest("GET", "https://release-assets.githubusercontent.com/item", nil)
	target.Header.Set("Authorization", "Bearer fixture")
	if err := releaseRedirect(target, []*http.Request{origin}); err != nil || target.Header.Get("Authorization") != "" {
		t.Fatal("custom repository redirect leaked token or failed")
	}
	origin.URL.Path = "/repos/" + Repository + "/releases/assets/5"
	if releaseRedirect(target, []*http.Request{origin}) == nil {
		t.Fatal("cross-repository asset redirect accepted")
	}
}

// sourceRunner provides the configured image metadata and records immutable command routing.
type sourceRunner struct {
	image string
	calls []string
}

func (runner *sourceRunner) Run(_ context.Context, name string, args, environment []string) ([]byte, error) {
	command := name + " " + strings.Join(args, " ")
	runner.calls = append(runner.calls, command)
	if strings.Contains(command, "image inspect") {
		return []byte(`{"Os":"linux","Architecture":"amd64","Config":{"Labels":{"org.opencontainers.image.version":"1.2.0"}},"RepoDigests":["` + runner.image + `@` + testRelease().Digest + `"]}`), nil
	}
	return nil, nil
}

// TestConfiguredDockerImageRoutes proves pull and tag use the snapshot, not a mutable global.
func TestConfiguredDockerImageRoutes(t *testing.T) {
	source := alternativeSource()
	runner := &sourceRunner{image: source.Image}
	host := &DockerHost{Config: Config{DeployDir: "/trusted"}, Runner: runner, trustCheck: func(string) error { return nil }}
	release := testRelease()
	release.Image = source.Image
	ctx := sourceContext(context.Background(), source)
	if err := host.Pull(ctx, release); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(runner.calls[1], "pull "+source.Image+"@") || !strings.HasSuffix(runner.calls[3], source.Image+":1.2.0") {
		t.Fatal("incorrect image routing")
	}
	if host.Pull(context.Background(), release) == nil {
		t.Fatal("cross-source image accepted")
	}
	release.Image = Image
	if host.Pull(ctx, release) == nil {
		t.Fatal("manifest image policy bypass")
	}
}

// TestCrossRepositorySameVersionRejected keeps semantic downgrade protection after source changes.
func TestCrossRepositorySameVersionRejected(t *testing.T) {
	release := testRelease()
	release.Image = alternativeSource().Image
	service := newTestService(t, &fakeHost{current: release.Version}, &fakeSource{release: release}, &memoryStore{})
	if _, err := service.SaveSource(alternativeSource()); err != nil {
		t.Fatal(err)
	}
	if err := service.Check(); err != nil {
		t.Fatal(err)
	}
	service.Wait()
	if service.Status().CanApply || service.Apply(release.Version, release.Digest, "更新 "+release.Version) == nil {
		t.Fatal("same version across repositories accepted")
	}
}
