package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestDesiredPorts checks allowlisting and deduplication without opening listeners.
func TestDesiredPorts(t *testing.T) {
	cfg := Config{Mappings: []Mapping{{Enabled: true, ListenHost: "0.0.0.0", ListenPort: 13306}, {Enabled: true, ListenHost: "0.0.0.0", ListenPort: 3306}, {Enabled: false, ListenPort: 6379}}}
	ports, err := desiredPorts(cfg)
	if err != nil || len(ports) != 2 || ports[0] != 3306 || ports[1] != 13306 {
		t.Fatal(ports, err)
	}
	for _, port := range []int{22, 6379, 8080, 18080, 65536} {
		cfg.Mappings[0].ListenPort = port
		if _, err := desiredPorts(cfg); err == nil {
			t.Fatalf("reserved port accepted: %d", port)
		}
	}
	cfg.Mappings[0].ListenPort = 13306
	cfg.Mappings[0].ListenHost = "127.0.0.1"
	if _, err := desiredPorts(cfg); err == nil {
		t.Fatal("loopback published")
	}
}

// TestPortsRouteAuthAndLegacy verifies safe downgrade without modifying legacy hosts.
func TestPortsRouteAuthAndLegacy(t *testing.T) {
	app := testApp(t)
	cookie, csrf := loginTest(t, app)
	if response := requestAPI(app, "GET", "/api/ports", "", nil, ""); response.Code != 401 {
		t.Fatal(response.Code)
	}
	if response := requestAPI(app, "POST", "/api/ports/apply", "{}", cookie, ""); response.Code != 403 {
		t.Fatal(response.Code)
	}
	app.onlineUpdates = fakeOnlineUpdater(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	response := requestAPI(app, "GET", "/api/ports", "", cookie, csrf)
	var status portPanelStatus
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if response.Code != 200 || status.Supported || status.CanApply {
		t.Fatal(response.Body.String())
	}
	if response := requestAPI(app, "GET", "/api/ports?ports=22", "", cookie, csrf); response.Code != 400 {
		t.Fatal(response.Code)
	}
}

// TestPortApplyRevisionAndBody confirms derived intent, no arbitrary ports and no repeated POST.
func TestPortApplyRevisionAndBody(t *testing.T) {
	app := testApp(t)
	cookie, csrf := loginTest(t, app)
	posts := 0
	app.onlineUpdates = fakeOnlineUpdater(t, func(w http.ResponseWriter, r *http.Request) {
		status := portPanelStatus{Supported: true, SchemaVersion: 1, Revision: "host-v1", State: "idle", CanApply: true, AppliedPorts: []int{}, ActualPorts: []int{}}
		if r.Method == "POST" {
			posts++
			var body struct {
				Ports            []int  `json:"ports"`
				Revision         string `json:"revision"`
				Confirmation     string `json:"confirmation"`
				FinalizePrevious bool   `json:"finalizePrevious"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.Revision != "host-v1" || len(body.Ports) != 0 || body.Confirmation != "应用端口变更" {
				t.Errorf("unexpected derived request: %+v", body)
			}
			status.State = "applying"
			status.OperationID = "operation-test"
			status.CanApply = false
			w.WriteHeader(202)
		}
		if err := json.NewEncoder(w).Encode(status); err != nil {
			t.Error(err)
		}
	})
	app.mu.Lock()
	revision := app.portRevision()
	app.mu.Unlock()
	body := `{"revision":"host-v1","configRevision":"` + revision + `","confirmation":"应用端口变更","finalizePrevious":false}`
	response := requestAPI(app, "POST", "/api/ports/apply", strings.Replace(body, revision, strings.Repeat("0", 64), 1), cookie, csrf)
	if response.Code != 409 || posts != 0 {
		t.Fatal(response.Code, posts)
	}
	response = requestAPI(app, "POST", "/api/ports/apply", strings.TrimSuffix(body, "}")+`,"ports":[22]}`, cookie, csrf)
	if response.Code != 400 || posts != 0 {
		t.Fatal(response.Code, posts)
	}
	response = requestAPI(app, "POST", "/api/ports/apply", body, cookie, csrf)
	if response.Code != 202 || posts != 1 {
		t.Fatal(response.Code, response.Body.String(), posts)
	}
}

// TestHostMutationGate checks fail-closed behavior during persistent operations and recovery.
func TestHostMutationGate(t *testing.T) {
	for _, phase := range []string{"applying", "manual_recovery", "idle"} {
		t.Run(phase, func(t *testing.T) {
			app := testApp(t)
			app.onlineUpdates = fakeOnlineUpdater(t, func(w http.ResponseWriter, r *http.Request) {
				state := phase
				if r.URL.Path == "/status" {
					state = "downloading"
				}
				writeJSON(w, 200, map[string]any{"supported": true, "schemaVersion": 1, "revision": "host-v1", "state": state})
			})
			app.mu.Lock()
			err := app.hostMutationAllowed()
			app.mu.Unlock()
			if err == nil {
				t.Fatal("configuration mutation accepted during host operation")
			}
		})
	}
}
