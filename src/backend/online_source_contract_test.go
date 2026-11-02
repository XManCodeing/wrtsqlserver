package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"opernwrtdocker/backend/internal/onlineupdate"
)

// TestOnlineSourceFullContract verifies authenticated source edits through the real Unix handler.
// Host and storage are fake; no registry, credentials, Docker or router are accessed.
func TestOnlineSourceFullContract(t *testing.T) {
	release := onlineupdate.Release{Version: "1.0.1", Image: onlineupdate.Image, Digest: "sha256:" + strings.Repeat("a", 64), Platform: onlineupdate.Platform}
	store := &contractStore{}
	host := &contractHost{current: "1.0.0"}
	service, err := onlineupdate.NewService(store, host, contractSource{release}, nil)
	if err != nil {
		t.Fatal(err)
	}
	app := testApp(t)
	app.onlineUpdates = fakeOnlineUpdater(t, http.HandlerFunc(service.Handler().ServeHTTP))
	cookie, csrf := loginTest(t, app)
	endpoint := "/api/online-updates/source"
	response := requestAPI(app, "GET", endpoint, "", cookie, "")
	var initial map[string]string
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &initial) != nil {
		t.Fatalf("source read failed: %d %s", response.Code, response.Body.String())
	}
	if initial["repositoryUrl"] != "https://github.com/XManCodeing/wrtsqlserver" || initial["image"] != onlineupdate.Image {
		t.Fatal("incorrect default source", initial)
	}
	if err = service.Check(); err != nil {
		t.Fatal(err)
	}
	service.Wait()
	if !service.Status().CanApply {
		t.Fatal("expected an available cached release")
	}
	body := `{"repositoryUrl":"https://github.com/example/relay-fork","image":"ghcr.io/example/relay-fork"}`
	for _, authentication := range []struct {
		authenticated bool
		expected      int
	}{
		{false, 401}, {true, 403},
	} {
		if !authentication.authenticated {
			response = requestAPI(app, "POST", endpoint, body, nil, "")
		} else {
			response = requestAPI(app, "POST", endpoint, body, cookie, "")
		}
		if response.Code != authentication.expected {
			t.Fatalf("source auth bypass: %d", response.Code)
		}
	}
	response = requestAPI(app, "POST", endpoint, body, cookie, csrf)
	if response.Code != 200 {
		t.Fatalf("source save failed: %d %s", response.Code, response.Body.String())
	}
	if status := service.Status(); status.Latest != nil || status.CanApply {
		t.Fatal("old release survived source change", status)
	}
	if err = service.Apply(release.Version, release.Digest, "更新 "+release.Version); err == nil {
		t.Fatal("stale update accepted after source change")
	}
	// Restart the real service on the same fake durable record and verify the saved selection.
	restarted, err := onlineupdate.NewService(store, host, contractSource{release}, nil)
	if err != nil {
		t.Fatal(err)
	}
	app.onlineUpdates = fakeOnlineUpdater(t, http.HandlerFunc(restarted.Handler().ServeHTTP))
	response = requestAPI(app, "GET", endpoint, "", cookie, "")
	var persisted map[string]string
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &persisted) != nil {
		t.Fatalf("persisted source read failed: %d", response.Code)
	}
	if persisted["repositoryUrl"] != "https://github.com/example/relay-fork" || persisted["image"] != "ghcr.io/example/relay-fork" {
		t.Fatal("source not retained after restart", persisted)
	}
}
