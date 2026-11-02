package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"opernwrtdocker/backend/internal/onlineupdate"
)

// fakeOnlineUpdater serves a private temporary Unix socket; returns its client without touching Docker or host services.
func fakeOnlineUpdater(t *testing.T, handler http.HandlerFunc) *onlineUpdateProxy {
	t.Helper()
	root, err := os.MkdirTemp("", "relay-updater-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	socket := filepath.Join(root, "control.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			t.Error(err)
		}
	}()
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Error(err)
		}
	})
	return newOnlineUpdateProxy(socket)
}

// TestOnlineUpdateRoutesAndAuth verifies the disabled contract and authentication boundaries on every operation.
func TestOnlineUpdateRoutesAndAuth(t *testing.T) {
	app := testApp(t)
	app.onlineUpdates = nil
	cookie, csrf := loginTest(t, app)
	for _, path := range []string{"/api/online-updates", "/api/online-updates/check", "/api/online-updates/apply", "/api/online-updates/download", "/api/online-updates/activate", "/api/online-updates/rollback"} {
		method := "POST"
		if path == "/api/online-updates" {
			method = "GET"
		}
		if response := requestAPI(app, method, path, "{}", nil, ""); response.Code != 401 {
			t.Fatal(path, response.Code)
		}
		if method == "POST" {
			if response := requestAPI(app, method, path, "{}", cookie, ""); response.Code != 403 {
				t.Fatal(path, response.Code)
			}
		}
		request := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader("{}"))
		request.AddCookie(cookie)
		request.Header.Set("Origin", "http://evil.invalid")
		request.Header.Set("X-CSRF-Token", csrf)
		writer := httptest.NewRecorder()
		app.ServeHTTP(writer, request)
		if writer.Code != 403 {
			t.Fatal(path, writer.Code)
		}
	}
	response := requestAPI(app, "GET", "/api/online-updates", "", cookie, "")
	var status map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if response.Code != 200 || status["enabled"] != false || status["state"] != "idle" || status["phase"] != "idle" || status["updatedAt"] != "" || len(status) != 10 {
		t.Fatal(status)
	}
	if response := requestAPI(app, "POST", "/api/online-updates/check", "", cookie, csrf); response.Code != 503 {
		t.Fatal(response.Code)
	}
}

// TestOnlineUpdateUnixContract ensures exact paths, body values, 202 preservation and no leaked session headers.
func TestOnlineUpdateUnixContract(t *testing.T) {
	app := testApp(t)
	cookie, csrf := loginTest(t, app)
	var count atomic.Int32
	payload := `{"enabled":true,"currentVersion":"1.0.0","latest":null,"state":"checking","phase":"check","message":"checking","operationId":"one","updatedAt":"2026-11-02T03:00:00Z","canApply":false,"canRollback":false}`
	app.onlineUpdates = fakeOnlineUpdater(t, func(writer http.ResponseWriter, request *http.Request) {
		count.Add(1)
		expectedMethods := map[string]string{"/status": "GET", "/check": "POST", "/apply": "POST", "/download": "POST", "/activate": "POST", "/rollback": "POST"}
		if expectedMethods[request.URL.Path] != request.Method || request.Host != "relay-updater" {
			t.Error("unexpected upstream route", request.Method, request.URL.Path)
		}
		if request.Header.Get("Cookie") != "" || request.Header.Get("X-CSRF-Token") != "" {
			t.Error("credentials forwarded")
		}
		if request.URL.Path == "/apply" || request.URL.Path == "/download" || request.URL.Path == "/activate" {
			var input map[string]string
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			if input["version"] != "v1.2.3" || input["confirmation"] != "更新 v1.2.3" || input["digest"] != "sha256:"+strings.Repeat("a", 64) {
				t.Error(input)
			}
		}
		writer.WriteHeader(202)
		if _, err := writer.Write([]byte(payload)); err != nil {
			t.Error(err)
		}
	})
	for _, operation := range []struct{ method, path, body string }{
		{"GET", "", ""}, {"POST", "/check", ""},
		{"POST", "/apply", `{"version":"v1.2.3","digest":"sha256:` + strings.Repeat("a", 64) + `","confirmation":"更新 v1.2.3"}`},
		{"POST", "/download", `{"version":"v1.2.3","digest":"sha256:` + strings.Repeat("a", 64) + `","confirmation":"更新 v1.2.3"}`},
		{"POST", "/activate", `{"version":"v1.2.3","digest":"sha256:` + strings.Repeat("a", 64) + `","confirmation":"更新 v1.2.3"}`},
		{"POST", "/rollback", `{"confirmation":"回退转发程序"}`},
	} {
		response := requestAPI(app, operation.method, "/api/online-updates"+operation.path, operation.body, cookie, csrf)
		if response.Code != 202 || response.Body.String() != payload {
			t.Fatal(response.Code, response.Body.String())
		}
	}
	if count.Load() != 6 {
		t.Fatal(count.Load())
	}
	for _, path := range []string{"/status", "/anything", "/check?url=http://evil.invalid"} {
		response := requestAPI(app, "POST", "/api/online-updates"+path, "{}", cookie, csrf)
		if response.Code != 404 && response.Code != 400 {
			t.Fatal(path, response.Code)
		}
	}
	for _, body := range []string{`{"url":"http://evil.invalid"}`, `{"version":"1.2.3","digest":"bad","confirmation":"更新 1.2.3"}`, `null`, `{}`} {
		if response := requestAPI(app, "POST", "/api/online-updates/apply", body, cookie, csrf); response.Code != 400 {
			t.Fatal(response.Code)
		}
	}
	if response := requestAPI(app, "GET", "/api/online-updates/check", "", cookie, csrf); response.Code != 405 {
		t.Fatal(response.Code)
	}
	if count.Load() != 6 {
		t.Fatal("invalid request reached updater")
	}
}

// TestOnlineUpdateRealSchema proxies actual updater Status values for every frontend fixture and preserves extension fields.
func TestOnlineUpdateRealSchema(t *testing.T) {
	fixture, err := os.ReadFile("../frontend/tests/online-update-stages.json")
	if err != nil {
		t.Fatal(err)
	}
	var stages []onlineupdate.Status
	if err := json.Unmarshal(fixture, &stages); err != nil {
		t.Fatal(err)
	}
	app := testApp(t)
	cookie, _ := loginTest(t, app)
	for _, stage := range stages {
		t.Run(stage.State+"/"+stage.Phase, func(t *testing.T) {
			stage.Enabled = true
			stage.CurrentVersion = "1.0.0"
			stage.OperationID = "operation-1"
			stage.UpdatedAt = "2026-11-02T03:00:00Z"
			stage.Latest = &onlineupdate.Release{Version: "1.2.3", Image: "ghcr.io/example/relay", Digest: "sha256:" + strings.Repeat("a", 64), Platform: "linux/amd64"}
			// Embed the real contract so schema changes cannot silently pass hand-authored payload tests.
			payload, err := json.Marshal(struct {
				onlineupdate.Status
				Extension string `json:"futureField"`
			}{Status: stage, Extension: "preserved"})
			if err != nil {
				t.Fatal(err)
			}
			app.onlineUpdates = fakeOnlineUpdater(t, func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(200)
				if _, err := writer.Write(payload); err != nil {
					t.Error(err)
				}
			})
			response := requestAPI(app, "GET", "/api/online-updates", "", cookie, "")
			if response.Code != 200 || response.Body.String() != string(payload) {
				t.Fatal(response.Code, response.Body.String())
			}
		})
	}
	// Disabled mode has exactly the same keys and JSON value types as the updater's zero snapshot.
	disabled, err := json.Marshal(disabledOnlineStatus())
	if err != nil {
		t.Fatal(err)
	}
	var typed onlineupdate.Status
	if err := json.Unmarshal(disabled, &typed); err != nil {
		t.Fatal(err)
	}
	if typed.State != "idle" || typed.Phase != "idle" || typed.Enabled {
		t.Fatal(typed)
	}
}

// TestOnlineSource verifies source auth/CSRF, validation, fixed Unix forwarding and fail-closed disabled/unavailable behavior.
func TestOnlineSource(t *testing.T) {
	app := testApp(t)
	cookie, csrf := loginTest(t, app)
	payload := `{"repositoryUrl":"https://github.com/XManCodeing/wrtsqlserver","image":"ghcr.io/xmancodeing/wrtsqlserver"}`
	path := "/api/online-updates/source"
	var count atomic.Int32
	app.onlineUpdates = fakeOnlineUpdater(t, func(writer http.ResponseWriter, request *http.Request) {
		count.Add(1)
		if request.URL.Path != "/source" {
			t.Error(request.URL.Path)
		}
		if request.Method == "POST" {
			var source onlineSource
			if err := json.NewDecoder(request.Body).Decode(&source); err != nil {
				t.Error(err)
			}
			if !validOnlineSource(source) {
				t.Error(source)
			}
		}
		writer.WriteHeader(200)
		if _, err := writer.Write([]byte(payload)); err != nil {
			t.Error(err)
		}
	})
	for _, method := range []string{"GET", "POST"} {
		if response := requestAPI(app, method, path, payload, nil, ""); response.Code != 401 {
			t.Fatal(response.Code)
		}
		request := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(payload))
		request.AddCookie(cookie)
		request.Header.Set("Origin", "https://evil.invalid")
		request.Header.Set("X-CSRF-Token", csrf)
		writer := httptest.NewRecorder()
		app.ServeHTTP(writer, request)
		if writer.Code != 403 {
			t.Fatal(writer.Code)
		}
		response := requestAPI(app, method, path, payload, cookie, csrf)
		if response.Code != 200 || response.Body.String() != payload {
			t.Fatal(response.Code, response.Body.String())
		}
	}
	if response := requestAPI(app, "POST", path, payload, cookie, ""); response.Code != 403 {
		t.Fatal(response.Code)
	}
	for _, body := range []string{
		`{}`, `null`, `{"repositoryUrl":"https://github.com/a/b","image":"ghcr.io/a/b","token":"secret"}`,
		`{"repositoryUrl":"http://github.com/a/b","image":"ghcr.io/a/b"}`,
		`{"repositoryUrl":"https://github.com/a/b?token=x","image":"ghcr.io/a/b"}`,
		`{"repositoryUrl":"https://github.com/a/b","image":"docker.io/a/b"}`,
		`{"repositoryUrl":"https://github.com/a/b","image":"ghcr.io/a/b:latest"}`,
		`{"repositoryUrl":"https://github.com/a/` + strings.Repeat("x", 257) + `","image":"ghcr.io/a/b"}`,
	} {
		if response := requestAPI(app, "POST", path, body, cookie, csrf); response.Code != 400 {
			t.Fatal(response.Code, body)
		}
	}
	if count.Load() != 2 {
		t.Fatal("invalid input reached updater", count.Load())
	}
	app.onlineUpdates = nil
	for _, method := range []string{"GET", "POST"} {
		if response := requestAPI(app, method, path, payload, cookie, csrf); response.Code != 503 {
			t.Fatal(response.Code)
		}
	}
	app.onlineUpdates = newOnlineUpdateProxy("/nonexistent-relay-updater/control.sock")
	if response := requestAPI(app, "POST", path, payload, cookie, csrf); response.Code != 502 {
		t.Fatal(response.Code)
	}
}

// TestOnlineUpdateResponseBounds proves oversize/malformed/errors/redirects are safe and timeout is bounded.
func TestOnlineUpdateResponseBounds(t *testing.T) {
	app := testApp(t)
	cookie, _ := loginTest(t, app)
	for _, scenario := range []string{"oversize", "malformed", "error", "redirect", "timeout"} {
		t.Run(scenario, func(t *testing.T) {
			app.onlineUpdates = fakeOnlineUpdater(t, func(writer http.ResponseWriter, request *http.Request) {
				switch scenario {
				case "oversize":
					writeJSON(writer, 200, map[string]string{"message": strings.Repeat("x", onlineResponseLimit)})
				case "malformed":
					writer.WriteHeader(200)
				case "error":
					writeJSON(writer, 500, map[string]string{"error": "secret internal path"})
				case "redirect":
					writer.Header().Set("Location", "http://evil.invalid")
					writeJSON(writer, 302, map[string]string{"error": "secret"})
				case "timeout":
					<-request.Context().Done()
				}
			})
			if app.onlineUpdates.client.Timeout != 10*time.Second {
				t.Fatal("incorrect production timeout")
			}
			if scenario == "timeout" {
				app.onlineUpdates.client.Timeout = 20 * time.Millisecond
			}
			response := requestAPI(app, "GET", "/api/online-updates", "", cookie, "")
			if response.Code != 502 || strings.Contains(response.Body.String(), "secret") {
				t.Fatal(response.Code, response.Body.String())
			}
		})
	}
	if newOnlineUpdateProxy("http://evil.invalid") != nil || newOnlineUpdateProxy("relative.sock") != nil {
		t.Fatal("accepted non-Unix address")
	}
}
