package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testApp constructs isolated encrypted storage and an application; cleanup stops all listeners.
func testApp(t *testing.T) *App {
	t.Helper()
	store, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app, err := newApp(store, "test-password-long", "127.0.0.1:8080", "", 32)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return app
}

// requestAPI invokes the handler with optional cookie and CSRF, returning its recorder.
func requestAPI(app *App, method, path, body string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		request.AddCookie(cookie)
	}
	if csrf != "" {
		request.Header.Set("X-CSRF-Token", csrf)
	}
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, request)
	return recorder
}

// loginTest signs in to app and returns the session cookie and CSRF token.
func loginTest(t *testing.T, app *App) (*http.Cookie, string) {
	t.Helper()
	response := requestAPI(app, "POST", "/api/login", `{"password":"test-password-long"}`, nil, "")
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	var payload map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	cookie := response.Result().Cookies()[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatal("insecure cookie")
	}
	return cookie, payload["csrfToken"]
}

// TestAuthCSRFAndOrigin verifies login, session, authentication, strict origins and logout invalidation.
func TestAuthCSRFAndOrigin(t *testing.T) {
	app := testApp(t)
	if response := requestAPI(app, "GET", "/api/proxies", "", nil, ""); response.Code != 401 {
		t.Fatal(response.Code)
	}
	if response := requestAPI(app, "GET", "/api/session", "", nil, ""); !strings.Contains(response.Body.String(), `"authenticated":false`) {
		t.Fatal(response.Body.String())
	}
	cookie, csrf := loginTest(t, app)
	if response := requestAPI(app, "POST", "/api/logout", "", cookie, ""); response.Code != 403 {
		t.Fatal(response.Code)
	}
	request := httptest.NewRequest("POST", "http://localhost/api/logout", nil)
	request.AddCookie(cookie)
	request.Header.Set("X-CSRF-Token", csrf)
	request.Header.Set("Origin", "https://evil.example")
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, request)
	if recorder.Code != 403 {
		t.Fatal(recorder.Code)
	}
	if response := requestAPI(app, "GET", "/api/session", "", cookie, ""); !strings.Contains(response.Body.String(), csrf) {
		t.Fatal(response.Body.String())
	}
	if response := requestAPI(app, "POST", "/api/logout", "", cookie, csrf); response.Code != 200 {
		t.Fatal(response.Code)
	}
	if response := requestAPI(app, "GET", "/api/status", "", cookie, ""); response.Code != 401 {
		t.Fatal(response.Code)
	}
}

// TestProxySecretsAndPersistence checks no password exposure, preservation, encrypted files and restart persistence.
func TestProxySecretsAndPersistence(t *testing.T) {
	app := testApp(t)
	cookie, csrf := loginTest(t, app)
	response := requestAPI(app, "POST", "/api/proxies", `{"name":"proxy","host":"127.0.0.1","port":1080,"username":"user","password":"super-secret"}`, cookie, csrf)
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	var proxy ProxyView
	if err := json.Unmarshal(response.Body.Bytes(), &proxy); err != nil {
		t.Fatal(err)
	}
	if !proxy.PasswordSet || strings.Contains(response.Body.String(), "super-secret") {
		t.Fatal(response.Body.String())
	}
	response = requestAPI(app, "PUT", "/api/proxies/"+proxy.ID, `{"name":"new","host":"localhost","port":1081,"username":"user","password":""}`, cookie, csrf)
	if response.Code != 200 || app.cfg.Proxies[0].Password != "super-secret" {
		t.Fatal(response.Body.String())
	}
	response = requestAPI(app, "GET", "/api/proxies", "", cookie, "")
	if strings.Contains(response.Body.String(), "super-secret") {
		t.Fatal("secret exposed")
	}
	data, err := os.ReadFile(filepath.Join(app.store.directory, "config.enc"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("super-secret")) {
		t.Fatal("plaintext at rest")
	}
	loaded, err := app.store.Load()
	if err != nil || loaded.Proxies[0].Password != "super-secret" {
		t.Fatal("persistence failed", err)
	}
	for _, name := range []string{"config.enc", "secret.key"} {
		info, err := os.Stat(filepath.Join(app.store.directory, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("permissions", err)
		}
	}
	response = requestAPI(app, "PUT", "/api/proxies/"+proxy.ID, `{"name":"new","host":"localhost","port":1081,"username":"","clearPassword":true}`, cookie, csrf)
	if response.Code != 200 || app.cfg.Proxies[0].Password != "" {
		t.Fatal(response.Body.String())
	}
}

// TestInputBoundaries exercises mapping addresses, ACL, listener collisions and host limits.
func TestInputBoundaries(t *testing.T) {
	cfg := Config{Proxies: []Proxy{{ID: "proxy"}}, Mappings: []Mapping{}}
	base := Mapping{ID: "mapping", Name: "DB", ListenHost: "127.0.0.1", ListenPort: 15432, TargetHost: "db.internal", TargetPort: 5432, ProxyID: "proxy", MaxConnections: 10, AllowedCIDRs: []string{"127.0.0.1/32"}}
	if err := validateMapping(base, cfg, "127.0.0.1:8080"); err != nil {
		t.Fatal(err)
	}
	cases := []func(*Mapping){
		func(value *Mapping) { value.ListenHost = "8.8.8.8" }, func(value *Mapping) { value.ListenHost = "localhost" },
		func(value *Mapping) { value.ListenHost = "::1" }, func(value *Mapping) { value.ListenPort = 8080 },
		func(value *Mapping) { value.TargetPort = 65536 }, func(value *Mapping) { value.MaxConnections = 0 },
		func(value *Mapping) { value.AllowedCIDRs = nil }, func(value *Mapping) { value.AllowedCIDRs = []string{"0.0.0.0/0"} },
		func(value *Mapping) { value.TargetHost = "bad/host" }, func(value *Mapping) { value.ProxyID = "missing" },
	}
	for index, mutate := range cases {
		mapping := base
		mutate(&mapping)
		if validateMapping(mapping, cfg, "127.0.0.1:8080") == nil {
			t.Fatalf("case %d accepted", index)
		}
	}
	// Listener collisions apply only when both records are enabled.
	base.Enabled = true
	existing := base
	existing.ID = "other"
	existing.ListenHost = "0.0.0.0"
	cfg.Mappings = []Mapping{existing}
	if validateMapping(base, cfg, "127.0.0.1:8080") == nil {
		t.Fatal("duplicate accepted")
	}
	for _, host := range []string{"", "host:123", strings.Repeat("a", 64) + ".com", "-bad.example"} {
		if validHost(host) {
			t.Fatal("invalid host accepted", host)
		}
	}
}

// TestLoginRateAndStrictJSON verifies bounded attempts and rejection of ambiguous payloads.
func TestLoginRateAndStrictJSON(t *testing.T) {
	app := testApp(t)
	for index := 0; index < 10; index++ {
		response := requestAPI(app, "POST", "/api/login", `{"password":"wrong"}`, nil, "")
		if response.Code != 401 {
			t.Fatal(response.Code)
		}
	}
	if response := requestAPI(app, "POST", "/api/login", `{"password":"wrong"}`, nil, ""); response.Code != 429 {
		t.Fatal(response.Code)
	}
	app = testApp(t)
	cookie, csrf := loginTest(t, app)
	for _, body := range []string{`null`, `{} {}`, `{"unknown":true}`, strings.Repeat("x", 40000)} {
		response := requestAPI(app, "POST", "/api/proxies", body, cookie, csrf)
		if response.Code != 400 {
			t.Fatal(response.Code, response.Body.String())
		}
	}
}
