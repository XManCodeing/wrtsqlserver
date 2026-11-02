package main

import (
	"reflect"
	"testing"
)

// TestMappingCopyEnableHandoff checks conflicts preserve stopped copies until the original releases its port.
func TestMappingCopyEnableHandoff(t *testing.T) {
	app, cookie, csrf, input := copyAPIFixture(t)
	input.Enabled = true
	original := saveCopyAPI(t, app, cookie, csrf, "POST", "/api/mappings", input)
	input.Enabled = false
	input.Name += " copy"
	copied := saveCopyAPI(t, app, cookie, csrf, "POST", "/api/mappings", input)
	input.Enabled = true
	response := requestAPI(app, "PUT", "/api/mappings/"+copied.ID, copyJSON(t, input), cookie, csrf)
	if response.Code != 400 {
		t.Fatal("copy enabled while original still owns listener", response.Code)
	}
	loaded, err := app.store.Load()
	if err != nil || !reflect.DeepEqual(loaded.Mappings[1], copied.Mapping) {
		t.Fatal("rejected enable changed persisted copy", err)
	}
	originalInput := input
	originalInput.Enabled, originalInput.Name = false, original.Name
	stopped := saveCopyAPI(t, app, cookie, csrf, "PUT", "/api/mappings/"+original.ID, originalInput)
	if stopped.Status != "stopped" {
		t.Fatal("original did not release listener", stopped)
	}
	started := saveCopyAPI(t, app, cookie, csrf, "PUT", "/api/mappings/"+copied.ID, input)
	if started.Status != "running" || !started.Enabled {
		t.Fatal("copy did not acquire released listener", started)
	}
	// Saving the active record itself must not mistake its own ID for a conflicting owner.
	started = saveCopyAPI(t, app, cookie, csrf, "PUT", "/api/mappings/"+copied.ID, input)
	if started.Status != "running" {
		t.Fatal("active self update rejected", started)
	}
}
