package onlineupdate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// roundTripFunc adapts fake HTTP responses without opening network connections.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

// testRelease returns an immutable policy-compliant fixture.
func testRelease() Release {
	return Release{"1.2.0", Image, "sha256:" + strings.Repeat("a", 64), Platform}
}

// TestVersions covers stable semantic comparison, malformed values and overflow.
func TestVersions(t *testing.T) {
	for _, test := range []struct {
		candidate, current string
		want               bool
	}{{"1.10.0", "1.9.9", true}, {"2.0.0", "1.99.99", true}, {"1.0.0", "1.0.0", false}, {"1.0.0", "2.0.0", false}, {"01.2.0", "1.0.0", false}, {"1.2.0-rc1", "1.0.0", false}, {"18446744073709551616.0.0", "1.0.0", false}, {"1.2.0", "unknown", false}} {
		if newer(test.candidate, test.current) != test.want {
			t.Errorf("comparison failed: %+v", test)
		}
	}
}

// TestPrivateReleaseAuthentication verifies authentication on both fixed GitHub API requests.
func TestPrivateReleaseAuthentication(t *testing.T) {
	requests := 0
	client := &http.Client{CheckRedirect: releaseRedirect, Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.URL.Host != "api.github.com" || request.Header.Get("Authorization") != "Bearer private-test-token" {
			t.Error("missing private release authentication")
		}
		body := `{"tag_name":"v1.2.0","draft":false,"prerelease":false,"assets":[{"id":12,"name":"relay-release.json","size":200,"url":"https://api.github.com/repos/XManCodeing/wrtsqlserver/releases/assets/12"}]}`
		if requests == 2 {
			data, _ := json.Marshal(testRelease())
			body = string(data)
			if request.Header.Get("Accept") != "application/octet-stream" {
				t.Error("asset API required")
			}
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: request}, nil
	})}
	github := &GitHub{Client: client, Token: func() (string, error) { return "private-test-token", nil }}
	release, err := github.Latest(context.Background())
	if err != nil || release != testRelease() || requests != 2 {
		t.Fatalf("release lookup failed: %v", err)
	}
}

// TestRedirectToken verifies credentials never reach release CDN hosts and arbitrary hosts fail closed.
func TestRedirectToken(t *testing.T) {
	for _, target := range []string{"https://release-assets.githubusercontent.com/item", "https://objects.githubusercontent.com/item", "https://evil.example/item", "http://release-assets.githubusercontent.com/item", "https://release-assets.githubusercontent.com:443/item"} {
		parsed, _ := url.Parse(target)
		request := &http.Request{URL: parsed, Header: http.Header{"Authorization": []string{"Bearer secret"}}}
		origin, _ := http.NewRequest("GET", "https://api.github.com/repos/"+Repository+"/releases/assets/12", nil)
		err := releaseRedirect(request, []*http.Request{origin})
		allowed := target == "https://release-assets.githubusercontent.com/item" || target == "https://objects.githubusercontent.com/item"
		if (err == nil) != allowed || request.Header.Get("Authorization") != "" {
			t.Errorf("redirect policy failed for %s", target)
		}
	}
}

// TestActualRedirectAuth exercises net/http redirect handling rather than just the callback.
func TestActualRedirectAuth(t *testing.T) {
	calls := 0
	client := &http.Client{CheckRedirect: releaseRedirect, Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		response := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`)), Request: request}
		if calls == 1 {
			response.StatusCode = 302
			response.Header.Set("Location", "https://release-assets.githubusercontent.com/item")
		} else if request.Header.Get("Authorization") != "" {
			t.Error("token leaked to asset CDN")
		}
		return response, nil
	})}
	github := &GitHub{Client: client, Token: func() (string, error) { return "private-token", nil }}
	if _, err := github.apiGet(context.Background(), "/repos/"+Repository+"/releases/assets/12", "application/octet-stream", 16384); err != nil || calls != 2 {
		t.Fatal(err)
	}
}

// TestManifestPolicy rejects cross-repository manifests, platforms, versions and over-sized JSON.
func TestManifestPolicy(t *testing.T) {
	for _, change := range []func(*Release){func(value *Release) { value.Image = "evil/image" }, func(value *Release) { value.Digest = "sha256:abc" }, func(value *Release) { value.Platform = "linux/arm64" }, func(value *Release) { value.Version = "1.0.0;id" }} {
		release := testRelease()
		change(&release)
		if validateRelease(release) == nil {
			t.Error("unsafe manifest accepted")
		}
	}
	var release Release
	for _, body := range []string{strings.Repeat(" ", 16385), `{"url":"https://evil.example"}`, `{} {}`} {
		if decodeStrict(strings.NewReader(body), 16384, &release) == nil {
			t.Error("unsafe JSON accepted")
		}
	}
}

// memoryStore models atomic state persistence, with fault injection for intent ordering tests.
type memoryStore struct {
	mu     sync.Mutex
	record Record
	exists bool
	fail   bool
}

func (store *memoryStore) Load() (Record, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if !store.exists {
		return Record{}, os.ErrNotExist
	}
	return store.record, nil
}
func (store *memoryStore) Save(record Record) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.fail {
		return errors.New("disk full")
	}
	store.record = record
	store.exists = true
	return nil
}

// fakeSource allows a blocked request to test mutual exclusion and offline status reads.
type fakeSource struct {
	release Release
	gate    chan struct{}
	calls   int
}

func (source *fakeSource) Latest(context.Context) (Release, error) {
	source.calls++
	if source.gate != nil {
		<-source.gate
	}
	return source.release, nil
}

// fakeHost records only logical host actions and simulates shell completion markers.
type fakeHost struct {
	current           string
	previous          string
	currentErr        bool
	stopAfterApply    bool
	stopAfterRollback bool
	pullErr           bool
	applyErr          bool
	autoRollback      bool
	transaction       bool
	applied           bool
	rolledBack        bool
	calls             []string
}

func (host *fakeHost) Current(context.Context) (string, error) {
	if host.currentErr {
		return "", errors.New("container unavailable")
	}
	return host.current, nil
}
func (host *fakeHost) Pull(context.Context, Release) error {
	host.calls = append(host.calls, "pull")
	if host.pullErr {
		return errors.New("pull failed")
	}
	return nil
}
func (host *fakeHost) Script(_ context.Context, mode string, release Release) error {
	host.calls = append(host.calls, mode)
	switch mode {
	case "apply":
		host.transaction = true
		if host.applyErr {
			host.rolledBack = host.autoRollback
			return errors.New("health failed")
		}
		host.applied = true
		host.previous = host.current
		host.current = release.Version
		host.currentErr = host.stopAfterApply
	case "rollback":
		host.rolledBack = true
		if host.previous != "" {
			host.current = host.previous
		}
		host.currentErr = host.stopAfterRollback
	case "finalize":
		host.transaction = false
		host.applied = false
		host.rolledBack = false
	}
	return nil
}
func (host *fakeHost) Marker(name string) bool {
	if name == "applied" {
		return host.applied
	}
	return host.rolledBack
}
func (host *fakeHost) HasTransaction() bool { return host.transaction }

// newTestService returns an isolated updater with no network, credentials or Docker access.
func newTestService(t *testing.T, host *fakeHost, source *fakeSource, store *memoryStore) *Service {
	t.Helper()
	service, err := NewService(store, host, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// TestConcurrentCheck verifies status never fetches and concurrent mutations are rejected.
func TestConcurrentCheck(t *testing.T) {
	source := &fakeSource{release: testRelease(), gate: make(chan struct{})}
	host := &fakeHost{current: "1.0.0"}
	service := newTestService(t, host, source, &memoryStore{})
	if err := service.Check(); err != nil {
		t.Fatal(err)
	}
	for count := 0; count < 10; count++ {
		service.Status()
	}
	if service.Check() == nil {
		t.Error("concurrent check accepted")
	}
	close(source.gate)
	service.Wait()
	if source.calls != 1 || !service.Status().CanApply {
		t.Fatal("unexpected release checks/state")
	}
}

// TestUpdateLifecycle covers success, pull failure, auto rollback and manual recovery.
func TestUpdateLifecycle(t *testing.T) {
	for _, test := range []struct {
		name                    string
		pullErr, applyErr, auto bool
		state                   string
	}{{"success", false, false, false, "succeeded"}, {"pull", true, false, false, "failed"}, {"auto", false, true, true, "rolled_back"}, {"manual", false, true, false, "manual_recovery"}} {
		t.Run(test.name, func(t *testing.T) {
			host := &fakeHost{current: "1.0.0", pullErr: test.pullErr, applyErr: test.applyErr, autoRollback: test.auto}
			service := newTestService(t, host, &fakeSource{release: testRelease()}, &memoryStore{})
			if err := service.Check(); err != nil {
				t.Fatal(err)
			}
			service.Wait()
			release := testRelease()
			if err := service.Apply(release.Version, release.Digest, "更新 "+release.Version); err != nil {
				t.Fatal(err)
			}
			service.Wait()
			if service.Status().State != test.state {
				t.Fatalf("wrong state: %+v", service.Status())
			}
			if test.pullErr && strings.Contains(strings.Join(host.calls, ","), "apply") {
				t.Error("apply ran after failed pull")
			}
			if test.state == "succeeded" {
				if !service.Status().CanRollback {
					t.Fatal("rollback lost")
				}
				if err := service.Rollback("回退转发程序"); err != nil {
					t.Fatal(err)
				}
				service.Wait()
				if service.Status().CurrentVersion != "1.0.0" {
					t.Fatal("prior version lost")
				}
			}
		})
	}
}

// TestRestartRecovery never replays persisted intent and retains successful rollback availability.
func TestRestartRecovery(t *testing.T) {
	for _, state := range []string{"checking", "downloading", "applying", "succeeded"} {
		store := &memoryStore{exists: true, record: Record{Status: Status{State: state, CanRollback: true, CurrentVersion: "1.0.0"}}}
		host := &fakeHost{current: "1.0.0", transaction: true, applied: true}
		service := newTestService(t, host, &fakeSource{}, store)
		if state != "succeeded" && service.Status().State != "manual_recovery" {
			t.Error("interruption not detected")
		}
		if len(host.calls) != 0 || !service.Status().CanRollback {
			t.Error("replayed operation or lost rollback")
		}
	}
}

// TestPersistenceFailure refuses work when the intent cannot be durably stored.
func TestPersistenceFailure(t *testing.T) {
	store := &memoryStore{}
	host := &fakeHost{current: "1.0.0"}
	source := &fakeSource{release: testRelease()}
	service := newTestService(t, host, source, store)
	store.fail = true
	if service.Check() == nil || source.calls != 0 || service.Status().Enabled {
		t.Fatal("unsafe operation despite persistence failure")
	}
}

// TestHTTPInput rejects arbitrary URLs, mismatched confirmations and unexpected JSON fields.
func TestHTTPInput(t *testing.T) {
	service := newTestService(t, &fakeHost{current: "1.0.0"}, &fakeSource{release: testRelease()}, &memoryStore{})
	for _, body := range []string{`{"url":"https://evil.example"}`, `{"version":"1.2.0","digest":"bad","confirmation":"yes"}`, `{} {}`} {
		request := httptest.NewRequest("POST", "/apply", strings.NewReader(body))
		response := httptest.NewRecorder()
		service.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusConflict {
			t.Errorf("unexpected status %d", response.Code)
		}
	}
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/status", nil))
	if response.Code != 200 {
		t.Fatal("status unavailable")
	}
}

// TestPermissionRejection ensures public, symlink and hardlinked files cannot supply secrets/state.
func TestPermissionRejection(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "token")
	if err := os.WriteFile(path, []byte("test-not-a-credential"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := privateRead(path, 4096); err == nil {
		t.Error("public/untrusted token accepted")
	}
	link := filepath.Join(directory, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := privateRead(link, 4096); err == nil {
		t.Error("symlink accepted")
	}
	hardlink := filepath.Join(directory, "hardlink")
	if err := os.Link(path, hardlink); err != nil {
		t.Fatal(err)
	}
	if _, err := privateRead(hardlink, 4096); err == nil {
		t.Error("hardlink accepted")
	}
}
