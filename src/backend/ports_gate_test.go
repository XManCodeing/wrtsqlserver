package main

import (
	"net/http"
	"testing"
)

// TestLegacyHostGate uses the actual old handler behavior: unknown GET returns 405, not 404.
func TestLegacyHostGate(t *testing.T) {
	for _, state := range []string{"idle", "downloading"} {
		t.Run(state, func(t *testing.T) {
			app := testApp(t)
			app.onlineUpdates = fakeOnlineUpdater(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/ports" {
					http.Error(w, "method not allowed", 405)
					return
				}
				writeJSON(w, 200, map[string]string{"state": state})
			})
			app.mu.Lock()
			err := app.hostMutationAllowed()
			app.mu.Unlock()
			if (err == nil) != (state == "idle") {
				t.Fatalf("legacy state %s: %v", state, err)
			}
		})
	}
}

// TestPortStatusValidation rejects malformed capabilities and unsafe actual port lists.
func TestPortStatusValidation(t *testing.T) {
	valid := portPanelStatus{Supported: true, SchemaVersion: 1, Revision: "r1", State: "idle", AppliedPorts: []int{3306, 13306}, ActualPorts: []int{3306, 13306}}
	if !validPortStatus(valid) {
		t.Fatal("valid contract rejected")
	}
	for _, ports := range [][]int{{22}, {6379}, {8080}, {3306, 3306}} {
		invalid := valid
		invalid.ActualPorts = ports
		if validPortStatus(invalid) {
			t.Fatalf("invalid ports accepted: %v", ports)
		}
	}
	invalid := valid
	invalid.State = "unknown"
	if validPortStatus(invalid) {
		t.Fatal("unknown state accepted")
	}
	invalid = valid
	invalid.SchemaVersion = 2
	if validPortStatus(invalid) {
		t.Fatal("unknown schema accepted")
	}
}
