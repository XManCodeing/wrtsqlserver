package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// copyTestInput returns valid writable configuration for proxyID and a local listener port.
func copyTestInput(proxyID string, port int) mappingInput {
	return mappingInput{Name: "数据库 copy", ListenHost: "127.0.0.1", ListenPort: port,
		TargetHost: "db.internal", TargetPort: 3306, ProxyID: proxyID, MaxConnections: 32,
		AllowedCIDRs: []string{"127.0.0.1/32"}}
}

// copyTestMapping converts writable input and id to a model for validation assertions.
func copyTestMapping(input mappingInput, id string) Mapping {
	return Mapping{ID: id, Name: input.Name, ListenHost: input.ListenHost, ListenPort: input.ListenPort,
		TargetHost: input.TargetHost, TargetPort: input.TargetPort, ProxyID: input.ProxyID,
		Enabled: input.Enabled, MaxConnections: input.MaxConnections, AllowedCIDRs: input.AllowedCIDRs}
}

// TestMappingCopyConflictMatrix checks enabled combinations, wildcard overlap, ports and self updates.
func TestMappingCopyConflictMatrix(t *testing.T) {
	for _, hosts := range [][2]string{{"127.0.0.1", "127.0.0.1"}, {"0.0.0.0", "127.0.0.1"},
		{"127.0.0.1", "0.0.0.0"}, {"0.0.0.0", "0.0.0.0"}, {"127.0.0.1", "127.0.0.2"}} {
		for _, enabled := range [][2]bool{{false, false}, {false, true}, {true, false}, {true, true}} {
			for _, samePort := range []bool{false, true} {
				name := fmt.Sprintf("%s-%s/%v/%v", hosts[0], hosts[1], enabled, samePort)
				t.Run(name, func(t *testing.T) {
					candidate := copyTestMapping(copyTestInput("proxy", 13306), "copy")
					original := candidate
					original.ID = "original"
					candidate.ListenHost, original.ListenHost = hosts[0], hosts[1]
					candidate.Enabled, original.Enabled = enabled[0], enabled[1]
					if !samePort {
						original.ListenPort++
					}
					cfg := Config{Proxies: []Proxy{{ID: "proxy"}}, Mappings: []Mapping{original}}
					overlap := hosts[0] == hosts[1] || hosts[0] == "0.0.0.0" || hosts[1] == "0.0.0.0"
					conflict := samePort && overlap && enabled[0] && enabled[1]
					if err := validateMapping(candidate, cfg, "127.0.0.1:8080"); (err != nil) != conflict {
						t.Fatalf("conflict=%v, error=%v", conflict, err)
					}
					candidate.ID = original.ID
					if err := validateMapping(candidate, cfg, "127.0.0.1:8080"); err != nil {
						t.Fatalf("self update rejected: %v", err)
					}
				})
			}
		}
	}
}

// TestMappingCopySafety checks disabled copies cannot bypass security or resource validation.
func TestMappingCopySafety(t *testing.T) {
	cases := map[string]func(*Mapping){
		"management port":  func(value *Mapping) { value.ListenPort = 8080 },
		"public address":   func(value *Mapping) { value.ListenHost = "8.8.8.8" },
		"IPv6 address":     func(value *Mapping) { value.ListenHost = "::1" },
		"missing proxy":    func(value *Mapping) { value.ProxyID = "missing" },
		"empty ACL":        func(value *Mapping) { value.AllowedCIDRs = nil },
		"open ACL":         func(value *Mapping) { value.AllowedCIDRs = []string{"0.0.0.0/0"} },
		"IPv6 ACL":         func(value *Mapping) { value.AllowedCIDRs = []string{"::1/128"} },
		"too many ACLs":    func(value *Mapping) { value.AllowedCIDRs = make([]string, 65) },
		"target port":      func(value *Mapping) { value.TargetPort = 65536 },
		"listener port":    func(value *Mapping) { value.ListenPort = 0 },
		"target host":      func(value *Mapping) { value.TargetHost = "bad/host" },
		"connections low":  func(value *Mapping) { value.MaxConnections = 0 },
		"connections high": func(value *Mapping) { value.MaxConnections = 4097 },
		"name bytes":       func(value *Mapping) { value.Name = strings.Repeat("汉", 34) },
		"name control":     func(value *Mapping) { value.Name = "bad\nname" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			candidate := copyTestMapping(copyTestInput("proxy", 13306), "copy")
			mutate(&candidate)
			cfg := Config{Proxies: []Proxy{{ID: "proxy"}}}
			if err := validateMapping(candidate, cfg, "127.0.0.1:8080"); err == nil {
				t.Fatal("unsafe disabled copy accepted")
			}
		})
	}
}

// copyAPIFixture returns isolated app, authorization and disabled input with an available local port.
func copyAPIFixture(t *testing.T) (*App, *http.Cookie, string, mappingInput) {
	t.Helper()
	app := testApp(t)
	cookie, csrf := loginTest(t, app)
	response := requestAPI(app, "POST", "/api/proxies", `{"name":"local","host":"127.0.0.1","port":1080}`, cookie, csrf)
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	var proxy ProxyView
	if err := json.Unmarshal(response.Body.Bytes(), &proxy); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return app, cookie, csrf, copyTestInput(proxy.ID, port)
}

// copyJSON serializes value for API tests, failing t on invalid input; returns JSON text.
func copyJSON(t *testing.T, value any) string {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// saveCopyAPI sends input to method/path and returns the successfully saved mapping view.
func saveCopyAPI(t *testing.T, app *App, cookie *http.Cookie, csrf, method, path string, input mappingInput) MappingView {
	t.Helper()
	response := requestAPI(app, method, path, copyJSON(t, input), cookie, csrf)
	if response.Code != 200 {
		t.Fatalf("save returned %d: %s", response.Code, response.Body.String())
	}
	var saved MappingView
	if err := json.Unmarshal(response.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	return saved
}

// TestMappingCopyAPIPersistence checks independent IDs, no runtime/history inheritance and restart loading.
func TestMappingCopyAPIPersistence(t *testing.T) {
	app, cookie, csrf, input := copyAPIFixture(t)
	input.Enabled, input.Name = true, "数据库"
	original := saveCopyAPI(t, app, cookie, csrf, "POST", "/api/mappings", input)
	if original.Status != "running" {
		t.Fatal("original listener did not start", original)
	}
	app.mu.Lock()
	originalRuntime := app.runtimes[original.ID]
	app.histories[original.ID] = []ConnectivityPoint{{}}
	app.mu.Unlock()
	input.Enabled, input.Name = false, "数据库 copy"
	copied := saveCopyAPI(t, app, cookie, csrf, "POST", "/api/mappings", input)
	if copied.ID == "" || copied.ID == original.ID || copied.Enabled || copied.Status != "stopped" || copied.ActiveConnections != 0 || copied.LastError != "" {
		t.Fatal("copy inherited identity/runtime", copied)
	}
	if !reflect.DeepEqual(copied.Mapping, copyTestMapping(input, copied.ID)) {
		t.Fatal("copy lost writable configuration")
	}
	app.mu.Lock()
	unchanged := app.runtimes[original.ID] == originalRuntime && len(app.histories[original.ID]) == 1 && len(app.histories[copied.ID]) == 0
	app.mu.Unlock()
	if !unchanged {
		t.Fatal("copy changed original runtime or inherited history")
	}
	loaded, err := app.store.Load()
	if err != nil || len(loaded.Mappings) != 2 || !reflect.DeepEqual(loaded.Mappings[1], copied.Mapping) {
		t.Fatal("copy not persisted", err)
	}
	app.Close()
	restarted, err := newApp(app.store, "test-password-long", app.apiAddr, "", 32)
	if err != nil {
		t.Fatal("disabled duplicate rejected on restart", err)
	}
	defer restarted.Close()
	if restarted.runtimes[copied.ID].view().Status != "stopped" || restarted.runtimes[original.ID].view().Status != "running" {
		t.Fatal("restart changed enabled state")
	}
}

// TestMappingCopyConcurrentCreatesAndEnables proves serialized updates allow copies but one active owner.
func TestMappingCopyConcurrentCreatesAndEnables(t *testing.T) {
	app, cookie, csrf, input := copyAPIFixture(t)
	const count = 6
	body := copyJSON(t, input)
	responses := make(chan *http.Response, count)
	var workers sync.WaitGroup
	for index := 0; index < count; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			responses <- requestAPI(app, "POST", "/api/mappings", body, cookie, csrf).Result()
		}()
	}
	workers.Wait()
	close(responses)
	ids := map[string]bool{}
	for response := range responses {
		var saved MappingView
		err := json.NewDecoder(response.Body).Decode(&saved)
		closeErr := response.Body.Close()
		if response.StatusCode != 200 || err != nil || closeErr != nil || saved.ID == "" || ids[saved.ID] || saved.Enabled {
			t.Fatalf("concurrent copy failed: code=%d error=%v close=%v", response.StatusCode, err, closeErr)
		}
		ids[saved.ID] = true
	}
	input.Enabled = true
	body = copyJSON(t, input)
	codes := make(chan int, count)
	for id := range ids {
		workers.Add(1)
		go func(id string) {
			defer workers.Done()
			codes <- requestAPI(app, "PUT", "/api/mappings/"+id, body, cookie, csrf).Code
		}(id)
	}
	workers.Wait()
	close(codes)
	successes := 0
	for code := range codes {
		if code == 200 {
			successes++
		} else if code != 400 {
			t.Fatal("unexpected enable result", code)
		}
	}
	if successes != 1 {
		t.Fatal("concurrent enables must have exactly one winner", successes)
	}
	loaded, err := app.store.Load()
	if err != nil || len(loaded.Mappings) != count {
		t.Fatal("concurrent mutations lost persisted copies", err)
	}
	enabled := 0
	for _, mapping := range loaded.Mappings {
		if mapping.Enabled {
			enabled++
		}
	}
	if enabled != 1 {
		t.Fatal("persisted competing listeners", enabled)
	}
}

// TestMappingCopyFailedSaveAndStrictFields checks atomic failure, runtime field rejection and port protection.
func TestMappingCopyFailedSaveAndStrictFields(t *testing.T) {
	app, cookie, csrf, input := copyAPIFixture(t)
	saved := saveCopyAPI(t, app, cookie, csrf, "POST", "/api/mappings", input)
	body := copyJSON(t, input)
	for _, field := range []string{`"id":"original"`, `"status":"running"`, `"activeConnections":9`, `"lastError":"old"`, `"history":[]`} {
		response := requestAPI(app, "POST", "/api/mappings", strings.TrimSuffix(body, "}")+","+field+"}", cookie, csrf)
		if response.Code != 400 {
			t.Fatal("server-owned field accepted", field, response.Code)
		}
	}
	for _, host := range []string{"127.0.0.1", "127.0.0.2", "0.0.0.0"} {
		unsafe := input
		unsafe.ListenHost, unsafe.ListenPort = host, 8080
		response := requestAPI(app, "POST", "/api/mappings", copyJSON(t, unsafe), cookie, csrf)
		if response.Code != 400 || !strings.Contains(response.Body.String(), "management port") {
			t.Fatal("disabled copy bypassed management protection", response.Code, response.Body.String())
		}
	}
	originalDirectory := app.store.directory
	app.store.directory = filepath.Join(originalDirectory, "missing")
	response := requestAPI(app, "POST", "/api/mappings", body, cookie, csrf)
	app.store.directory = originalDirectory
	if response.Code != 500 {
		t.Fatal("save failure not surfaced", response.Code)
	}
	app.mu.Lock()
	unchanged := len(app.cfg.Mappings) == 1 && len(app.runtimes) == 1 && app.cfg.Mappings[0].ID == saved.ID
	app.mu.Unlock()
	if !unchanged {
		t.Fatal("failed copy changed live configuration")
	}
	loaded, err := app.store.Load()
	if err != nil || len(loaded.Mappings) != 1 {
		t.Fatal("failed copy changed durable configuration", err)
	}
}
