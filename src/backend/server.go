package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var version = "1.0.0"

// App serializes durable mutations while runtimes independently handle network traffic.
type App struct {
	mu           sync.Mutex
	cfg          Config
	store        *Store
	auth         *Auth
	apiAddr      string
	webDir       string
	started      time.Time
	runtimes     map[string]*Runtime
	slots        chan struct{}
	active       atomic.Int64
	testSlots    chan struct{}
	testRate     rateBucket
	histories    map[string][]ConnectivityPoint
	probeCtx     context.Context
	probeCancel  context.CancelFunc
	probeWait    sync.WaitGroup
	probeJobs    chan *Runtime
	probePending map[*Runtime]bool
	closed       bool
	updates      *updateManager
	redis        *redisMonitor
}

// newApp loads and validates durable configuration before starting any mapping listener.
func newApp(store *Store, password, apiAddr, webDir string, maxConnections int) (*App, error) {
	cfg, err := store.Load()
	if err != nil {
		return nil, err
	}
	if len(cfg.Proxies) > 256 || len(cfg.Mappings) > connectivityQueueSize {
		return nil, errors.New("configuration exceeds proxy or mapping limit")
	}
	ids := map[string]bool{}
	for _, proxy := range cfg.Proxies {
		if proxy.ID == "" || ids[proxy.ID] {
			return nil, errors.New("invalid duplicate proxy ID")
		}
		ids[proxy.ID] = true
		if err := validateProxyCredentials(proxy); err != nil {
			return nil, err
		}
		if err := validateProxy(ProxyInput{Name: proxy.Name, Host: proxy.Host, Port: proxy.Port, Username: proxy.Username, Password: proxy.Password}); err != nil {
			return nil, err
		}
	}
	ids = map[string]bool{}
	for _, mapping := range cfg.Mappings {
		if mapping.ID == "" || ids[mapping.ID] {
			return nil, errors.New("invalid duplicate mapping ID")
		}
		ids[mapping.ID] = true
		if err := validateMapping(mapping, cfg, apiAddr); err != nil {
			return nil, err
		}
	}
	app := &App{cfg: cfg, store: store, auth: newAuth(password), apiAddr: apiAddr, webDir: webDir, started: time.Now(), runtimes: map[string]*Runtime{}, slots: make(chan struct{}, maxConnections), testSlots: make(chan struct{}, 4)}
	app.redis = newRedisMonitor(os.Getenv("REDIS_ADDR"))
	app.updates = newUpdateManager(os.Getenv("UPDATE_PUBLIC_KEY_FILE"), os.Getenv("UPDATE_STAGING_DIR"))
	app.reconcile(Config{Proxies: []Proxy{}, Mappings: []Mapping{}})
	app.startConnectivity()
	return app, nil
}

// proxyFor returns the endpoint referenced by id; callers validate references before use.
func proxyFor(cfg Config, id string) Proxy {
	for _, proxy := range cfg.Proxies {
		if proxy.ID == id {
			return proxy
		}
	}
	return Proxy{}
}

// reconcile restarts only affected mappings after persistence; binding failures remain visible as error states.
// The caller owns app.mu, so reads cannot observe an intermediate configuration.
func (app *App) reconcile(previous Config) {
	desired := map[string]Mapping{}
	for _, mapping := range app.cfg.Mappings {
		desired[mapping.ID] = mapping
	}
	for id, runtime := range app.runtimes {
		mapping, exists := desired[id]
		if !exists || connectivityChanged(runtime.mapping, mapping, runtime.proxy, proxyFor(app.cfg, mapping.ProxyID)) {
			delete(app.histories, id)
		}
		if !exists || !reflect.DeepEqual(mapping, runtime.mapping) || !reflect.DeepEqual(proxyFor(previous, runtime.mapping.ProxyID), proxyFor(app.cfg, mapping.ProxyID)) {
			runtime.Stop()
			delete(app.runtimes, id)
		}
	}
	for _, mapping := range app.cfg.Mappings {
		if _, exists := app.runtimes[mapping.ID]; !exists {
			app.runtimes[mapping.ID] = newRuntime(mapping, proxyFor(app.cfg, mapping.ProxyID), app.slots, &app.active)
		}
	}
}

// commit durably saves a candidate before changing in-memory configuration or running listeners.
func (app *App) commit(candidate Config) error {
	if err := app.store.Save(candidate); err != nil {
		log.Printf("configuration persistence failed")
		return errors.New("configuration could not be saved")
	}
	previous := app.cfg
	app.cfg = candidate
	app.reconcile(previous)
	return nil
}

// Close stops all mapping listeners and interrupts their streams during shutdown.
func (app *App) Close() {
	app.mu.Lock()
	app.closed = true
	if app.probeCancel != nil {
		app.probeCancel()
	}
	runtimes := make([]*Runtime, 0, len(app.runtimes))
	for _, runtime := range app.runtimes {
		runtime.cancel()
		runtimes = append(runtimes, runtime)
	}
	app.mu.Unlock()
	if app.redis != nil {
		app.redis.close()
	}
	// Workers record results under app.mu: never wait for them while owning it.
	for _, runtime := range runtimes {
		runtime.Stop()
	}
	app.probeWait.Wait()
}

// writeJSON serializes a response with no credential caching; encoding failures are logged without payloads.
func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		log.Print("HTTP response write failed")
	}
}

// apiError emits the stable API error envelope with an HTTP status.
func apiError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, map[string]string{"error": message})
}

// decodeJSON bounds input, rejects unknown fields and forbids concatenated or null JSON values.
func decodeJSON(writer http.ResponseWriter, request *http.Request, target any) bool {
	contentType := strings.Split(request.Header.Get("Content-Type"), ";")[0]
	if contentType != "application/json" {
		apiError(writer, 415, "Content-Type must be application/json")
		return false
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 32<<10)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	raw := json.RawMessage{}
	if err := decoder.Decode(&raw); err != nil || string(raw) == "null" {
		apiError(writer, 400, "invalid JSON body")
		return false
	}
	strict := json.NewDecoder(strings.NewReader(string(raw)))
	strict.DisallowUnknownFields()
	if err := strict.Decode(target); err != nil {
		apiError(writer, 400, "invalid or unknown JSON fields")
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		apiError(writer, 400, "only one JSON object is allowed")
		return false
	}
	return true
}

// ServeHTTP routes the fixed API contract and static assets with security headers.
func (app *App) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("X-Frame-Options", "DENY")
	writer.Header().Set("Referrer-Policy", "same-origin")
	writer.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'")
	path := request.URL.Path
	if path != "/api" && !strings.HasPrefix(path, "/api/") {
		app.static(writer, request)
		return
	}
	if !sameOrigin(request) {
		apiError(writer, 403, "invalid request origin")
		return
	}
	if path == "/api/session" && request.Method == "GET" {
		value, ok := app.auth.current(request)
		response := map[string]any{"authenticated": ok}
		if ok {
			response["csrfToken"] = value.csrf
		}
		writeJSON(writer, 200, response)
		return
	}
	if path == "/api/login" && request.Method == "POST" {
		app.handleLogin(writer, request)
		return
	}
	if !app.auth.authorize(writer, request) {
		return
	}
	if path == "/api/logout" && request.Method == "POST" {
		cookie, _ := request.Cookie("admin_session")
		app.auth.mu.Lock()
		delete(app.auth.sessions, cookie.Value)
		app.auth.mu.Unlock()
		setSessionCookie(writer, request, "", -1)
		writeJSON(writer, 200, map[string]bool{"ok": true})
		return
	}
	if path == "/api/updates" || strings.HasPrefix(path, "/api/updates/") {
		app.handleUpdates(writer, request)
		return
	}
	if path == "/api/status" && request.Method == "GET" {
		app.status(writer)
		return
	}
	if strings.HasPrefix(path, "/api/redis/databases/") {
		app.handleRedisData(writer, request)
		return
	}
	if path == "/api/redis/status" && request.Method == "GET" {
		writeRedisJSON(writer, app.redis.snapshot(request.Context()))
		return
	}
	if path == "/api/connectivity" && request.Method == "GET" {
		writeJSON(writer, 200, app.connectivitySnapshot(time.Now()))
		return
	}
	parts := strings.Split(strings.TrimPrefix(path, "/api/"), "/")
	if len(parts) == 3 && parts[0] == "mappings" && parts[2] == "test" && request.Method == "POST" {
		app.testMapping(writer, request, parts[1])
		return
	}
	if len(parts) <= 2 && (parts[0] == "proxies" || parts[0] == "mappings") {
		id := ""
		if len(parts) == 2 {
			id = parts[1]
			if id == "" {
				apiError(writer, 404, "resource not found")
				return
			}
		}
		app.crud(writer, request, parts[0], id)
		return
	}
	apiError(writer, 404, "API endpoint not found")
}

// handleLogin limits attempts before decoding passwords and returns a fresh CSRF token.
func (app *App) handleLogin(writer http.ResponseWriter, request *http.Request) {
	if !app.auth.allowLogin(request.RemoteAddr) {
		writer.Header().Set("Retry-After", "60")
		apiError(writer, 429, "too many login attempts")
		return
	}
	input := struct {
		Password string `json:"password"`
	}{}
	if !decodeJSON(writer, request, &input) {
		return
	}
	if len(input.Password) > 4096 {
		apiError(writer, 400, "password too long")
		return
	}
	token, err := app.auth.login(writer, request, input.Password)
	if err != nil {
		apiError(writer, 401, "login failed")
		return
	}
	writeJSON(writer, 200, map[string]string{"csrfToken": token})
}

// status returns uptime and race-free aggregate connection/listener metrics.
func (app *App) status(writer http.ResponseWriter) {
	app.mu.Lock()
	defer app.mu.Unlock()
	running := 0
	for _, runtime := range app.runtimes {
		if runtime.view().Status == "running" {
			running++
		}
	}
	writeJSON(writer, 200, map[string]any{"uptimeSeconds": int64(time.Since(app.started).Seconds()), "activeConnections": app.active.Load(), "runningMappings": running, "version": version})
}

// testMapping performs only a SOCKS5 connect, with global rate/concurrency and timeout limits.
func (app *App) testMapping(writer http.ResponseWriter, request *http.Request, id string) {
	app.mu.Lock()
	runtime, exists := app.runtimes[id]
	if !exists {
		app.mu.Unlock()
		apiError(writer, 404, "mapping not found")
		return
	}
	if !allowBucket(&app.testRate, time.Now(), 20, time.Minute) {
		app.mu.Unlock()
		apiError(writer, 429, "too many connection tests")
		return
	}
	app.mu.Unlock()
	select {
	case app.testSlots <- struct{}{}:
		defer func() { <-app.testSlots }()
	default:
		apiError(writer, 429, "connection test concurrency limit reached")
		return
	}
	app.manualConnectivity(writer, request, runtime)
}

// static serves regular build files and an SPA fallback, never falling back for /api routes.
func (app *App) static(writer http.ResponseWriter, request *http.Request) {
	if request.Method != "GET" && request.Method != "HEAD" {
		apiError(writer, 405, "method not allowed")
		return
	}
	if app.webDir == "" {
		http.NotFound(writer, request)
		return
	}
	clean := filepath.Clean("/" + request.URL.Path)
	file := filepath.Join(app.webDir, clean)
	info, err := os.Stat(file)
	if err != nil || !info.Mode().IsRegular() {
		file = filepath.Join(app.webDir, "index.html")
	}
	http.ServeFile(writer, request, file)
}
