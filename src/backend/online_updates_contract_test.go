package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"opernwrtdocker/backend/internal/onlineupdate"
)

// contractStore is an in-memory durable-store substitute; never touches host state.
type contractStore struct {
	mu     sync.Mutex
	record onlineupdate.Record
	exists bool
}

// Load returns the saved record or os.ErrNotExist before the first save.
func (store *contractStore) Load() (onlineupdate.Record, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if !store.exists {
		return onlineupdate.Record{}, os.ErrNotExist
	}
	return store.record, nil
}

// Save retains the supplied record and returns nil without filesystem effects.
func (store *contractStore) Save(record onlineupdate.Record) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.record = record
	store.exists = true
	return nil
}

// contractHost records only simulated container effects for the full HTTP contract test.
type contractHost struct {
	mu          sync.Mutex
	current     string
	transaction bool
	applied     bool
	rolled      bool
}

// Current returns the fake running version; context is unused because no IO occurs.
func (host *contractHost) Current(context.Context) (string, error) {
	host.mu.Lock()
	defer host.mu.Unlock()
	return host.current, nil
}

// Pull accepts the fake release and returns success without registry access.
func (host *contractHost) Pull(context.Context, onlineupdate.Release) error { return nil }

// Script applies a known simulated mode and returns nil; no shell is executed.
func (host *contractHost) Script(_ context.Context, mode string, release onlineupdate.Release) error {
	host.mu.Lock()
	defer host.mu.Unlock()
	switch mode {
	case "apply":
		host.current = release.Version
		host.transaction = true
		host.applied = true
	case "rollback":
		host.current = "1.0.0"
		host.applied = false
		host.rolled = true
	case "finalize":
		host.transaction = false
		host.applied = false
		host.rolled = false
	}
	return nil
}

// Marker reports whether the named completion marker exists in simulated state.
func (host *contractHost) Marker(name string) bool {
	host.mu.Lock()
	defer host.mu.Unlock()
	return name == "applied" && host.applied || name == "rolled-back" && host.rolled
}

// HasTransaction reports a retained simulated transaction without filesystem access.
func (host *contractHost) HasTransaction() bool {
	host.mu.Lock()
	defer host.mu.Unlock()
	return host.transaction
}

// contractSource returns an approved pinned release without internet access.
type contractSource struct{ release onlineupdate.Release }

// Latest returns the fixed release without using the context or making network calls.
func (source contractSource) Latest(context.Context) (onlineupdate.Release, error) {
	return source.release, nil
}

// TestOnlineUpdateFullContract passes the real service through Unix HTTP and authenticated app routes.
// It exercises check/apply/rollback without a real Docker daemon or actual registry.
func TestOnlineUpdateFullContract(t *testing.T) {
	release := onlineupdate.Release{Version: "1.0.1", Image: onlineupdate.Image, Digest: "sha256:" + strings.Repeat("a", 64), Platform: onlineupdate.Platform}
	host := &contractHost{current: "1.0.0"}
	service, err := onlineupdate.NewService(&contractStore{}, host, contractSource{release}, nil)
	if err != nil {
		t.Fatal(err)
	}
	app := testApp(t)
	app.onlineUpdates = fakeOnlineUpdater(t, http.HandlerFunc(service.Handler().ServeHTTP))
	cookie, csrf := loginTest(t, app)
	// Await only the fake asynchronous task, then read the actual serialized status through the proxy.
	await := func(expected string) onlineupdate.Status {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			response := requestAPI(app, "GET", "/api/online-updates", "", cookie, "")
			var status onlineupdate.Status
			if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &status) != nil {
				t.Fatalf("invalid status: %d", response.Code)
			}
			if status.State == expected {
				return status
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("state did not become %s", expected)
		return onlineupdate.Status{}
	}
	if response := requestAPI(app, "POST", "/api/online-updates/check", "{}", cookie, csrf); response.Code != 202 {
		t.Fatal(response.Code, response.Body.String())
	}
	available := await("available")
	if !available.CanApply || available.Phase != "ready" || available.Latest.Version != "1.0.1" {
		t.Fatal(available)
	}
	body, err := json.Marshal(map[string]string{"version": release.Version, "digest": release.Digest, "confirmation": "更新 " + release.Version})
	if err != nil {
		t.Fatal(err)
	}
	if response := requestAPI(app, "POST", "/api/online-updates/apply", string(body), cookie, csrf); response.Code != 202 {
		t.Fatal(response.Code, response.Body.String())
	}
	succeeded := await("succeeded")
	if succeeded.CurrentVersion != "1.0.1" || !succeeded.CanRollback || succeeded.CanApply {
		t.Fatal(succeeded)
	}
	if response := requestAPI(app, "POST", "/api/online-updates/rollback", `{"confirmation":"回退转发程序"}`, cookie, csrf); response.Code != 202 {
		t.Fatal(response.Code, response.Body.String())
	}
	rolled := await("rolled_back")
	if rolled.CurrentVersion != "1.0.0" {
		t.Fatal(rolled)
	}
}
