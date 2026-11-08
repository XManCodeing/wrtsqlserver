package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"opernwrtdocker/backend/internal/onlineupdate"
)

// checkTransport injects public GitHub responses without performing network or host operations.
type checkTransport func(*http.Request) (*http.Response, error)

// RoundTrip maps request to a deterministic response; its error follows http.RoundTripper.
func (transport checkTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

// mockPublicChecker returns a real GitHub validator backed by supplied metadata and manifest responses.
// t reports unexpected destinations/credentials, current is the local build version and manifest is asset JSON.
func mockPublicChecker(t *testing.T, current, manifest string) (*onlineReleaseChecker, *atomic.Int32) {
	t.Helper()
	checker := newOnlineReleaseChecker(current)
	calls := &atomic.Int32{}
	github := checker.source.(*onlineupdate.GitHub)
	github.Client.Transport = checkTransport(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		if request.URL.Scheme != "https" || request.URL.Host != "api.github.com" || request.Header.Get("Authorization") != "" {
			t.Errorf("unsafe request destination or credential: %s", request.URL.Host)
		}
		body := manifest
		if request.URL.Path == "/repos/XManCodeing/wrtsqlserver/releases/latest" {
			body = `{"tag_name":"v1.2.3","assets":[{"id":123,"name":"relay-release.json","size":300,"url":"https://api.github.com/repos/XManCodeing/wrtsqlserver/releases/assets/123"}]}`
		} else if request.URL.Path != "/repos/XManCodeing/wrtsqlserver/releases/assets/123" {
			t.Errorf("unexpected request path: %s", request.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})
	return checker, calls
}

// publicManifest returns an approved default-source manifest for read-only checker tests.
func publicManifest() string {
	return `{"version":"1.2.3","image":"ghcr.io/xmancodeing/wrtsqlserver","digest":"sha256:` + strings.Repeat("a", 64) + `","platform":"linux/amd64"}`
}

// TestPublicCheckVersionOrdering rejects distro-build downgrades and uncertain local versions.
func TestPublicCheckVersionOrdering(t *testing.T) {
	for _, scenario := range []struct {
		candidate, current string
		newer, comparable  bool
	}{
		{"1.2.2", "1.2.3-ubuntu", false, true}, {"1.2.3", "1.2.3-ubuntu", true, true},
		{"1.2.4", "1.2.3-ubuntu", true, true}, {"1.10.0", "v1.9.9+build", false, false},
		{"1.2.3", "1.2.3-custom", false, false}, {"1.2.4", "1.2.3-custom", false, false},
		{"1.2.3", "1.2.3-ubuntu+build", false, false}, {"1.2.3", "1.2.2-ubuntu", false, false},
		{"1.2.3", "unknown", false, false}, {"1.2.3", "01.2.2", false, false},
		{"1.2.3", "1.2.18446744073709551616", false, false},
	} {
		newer, comparable := onlineReleaseNewer(scenario.candidate, scenario.current)
		if newer != scenario.newer || comparable != scenario.comparable {
			t.Fatalf("%+v: %v %v", scenario, newer, comparable)
		}
	}
}

// TestPublicCheckValidation reuses actual GitHub validation and never grants host capabilities.
func TestPublicCheckValidation(t *testing.T) {
	for _, scenario := range []struct{ name, current, manifest, state string }{
		{"newer", "1.2.2", publicManifest(), "available"},
		{"bootstrap promotion", "1.2.3-ubuntu", publicManifest(), "available"},
		{"same stable", "1.2.3", publicManifest(), "idle"},
		{"older", "1.2.4-ubuntu", publicManifest(), "idle"},
		{"wrong image", "1.2.2", strings.ReplaceAll(publicManifest(), "xmancodeing/wrtsqlserver", "evil/relay"), "failed"},
		{"tag mismatch", "1.2.2", strings.ReplaceAll(publicManifest(), "1.2.3", "1.2.4"), "failed"},
		{"oversize", "1.2.2", strings.Repeat("x", 16*1024+1), "failed"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			checker, calls := mockPublicChecker(t, scenario.current, scenario.manifest)
			_ = checker.snapshot()
			if calls.Load() != 0 {
				t.Fatal("status triggered network")
			}
			checker.check()
			status := checker.snapshot()
			if status.State != scenario.state || !status.CheckOnly || status.Enabled || status.CanApply || status.CanRollback || status.StagedUpdates {
				t.Fatalf("unexpected capabilities: %+v", status)
			}
			if status.Latest != nil {
				status.Latest.Version = "mutated"
				if checker.snapshot().Latest.Version == "mutated" {
					t.Fatal("shared release pointer")
				}
			}
		})
	}
}

// TestPublicCheckRoutes verifies authenticated fixed-source reads, mutation rejection and explicit async detection.
func TestPublicCheckRoutes(t *testing.T) {
	checker, calls := mockPublicChecker(t, "1.2.2", publicManifest())
	previous := publicOnlineChecker
	publicOnlineChecker = checker
	t.Cleanup(func() { publicOnlineChecker = previous })
	app := testApp(t)
	app.onlineUpdates = nil
	cookie, csrf := loginTest(t, app)
	for _, path := range []string{"", "/source"} {
		response := requestAPI(app, "GET", "/api/online-updates"+path, "", cookie, "")
		if response.Code != 200 {
			t.Fatal(response.Code, response.Body.String())
		}
	}
	if calls.Load() != 0 {
		t.Fatal("reads must not contact GitHub")
	}
	for _, action := range []string{"source", "apply", "download", "activate", "rollback"} {
		if response := requestAPI(app, "POST", "/api/online-updates/"+action, "{}", cookie, csrf); response.Code != 503 {
			t.Fatal(action, response.Code)
		}
	}
	for _, body := range []string{`{"url":"http://127.0.0.1"}`, `{"token":"secret"}`, `null`} {
		if response := requestAPI(app, "POST", "/api/online-updates/check", body, cookie, csrf); response.Code != 400 {
			t.Fatal(response.Code)
		}
	}
	if response := requestAPI(app, "POST", "/api/online-updates/check?url=x", "", cookie, csrf); response.Code != 400 {
		t.Fatal(response.Code)
	}
	response := requestAPI(app, "POST", "/api/online-updates/check", "{}", cookie, csrf)
	var accepted onlineCheckStatus
	if response.Code != 202 || json.Unmarshal(response.Body.Bytes(), &accepted) != nil || accepted.State != "checking" {
		t.Fatal(response.Code, response.Body.String())
	}
	deadline := time.After(time.Second)
	for checker.snapshot().State == "checking" {
		select {
		case <-deadline:
			t.Fatal("check did not finish")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if checker.snapshot().State != "available" || calls.Load() != 2 {
		t.Fatal(checker.snapshot(), calls.Load())
	}
}

// TestPublicCheckNetworkPolicy verifies anonymous credentials, disabled proxies and bounded requests/headers.
func TestPublicCheckNetworkPolicy(t *testing.T) {
	checker := newOnlineReleaseChecker("1.0.0")
	github := checker.source.(*onlineupdate.GitHub)
	if token, err := github.Token(); err != nil || token != "" {
		t.Fatal("public checker read credentials")
	}
	transport := github.Client.Transport.(*http.Transport)
	if github.Client.Timeout != 12*time.Second || transport.Proxy != nil || transport.MaxResponseHeaderBytes != 8<<10 {
		t.Fatal("unbounded client")
	}
	github.Client.Transport = checkTransport(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := github.Latest(ctx); err == nil {
		t.Fatal("cancelled request accepted")
	}
	github.Client.Transport = checkTransport(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": {"http://127.0.0.1/private"}}, Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
	})
	if _, err := github.Latest(context.Background()); err == nil {
		t.Fatal("unsafe redirect accepted")
	}
	writer := httptest.NewRecorder()
	handleReadOnlyOnline(writer, httptest.NewRequest("POST", "/check", strings.NewReader(`{"url":"http://evil.invalid"}`)), "/check")
	if writer.Code != 400 {
		t.Fatal(writer.Code)
	}
}
