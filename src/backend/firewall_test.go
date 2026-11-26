package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// assertFirewall checks the complete wire contract, including no extra keys and a real 0600 file.
func assertFirewall(t *testing.T, app *App, expected ...firewallListener) {
	t.Helper()
	path := filepath.Join(app.store.directory, firewallFilename)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		t.Fatalf("missing or insecure snapshot: %v, %v", info, err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var snapshot firewallSnapshot
	if err := decoder.Decode(&snapshot); err != nil {
		t.Fatal(err)
	}
	if expected == nil {
		expected = []firewallListener{}
	}
	if !json.Valid(body) || snapshot.Version != 1 || !reflect.DeepEqual(snapshot.Listeners, expected) {
		t.Fatalf("unexpected snapshot: %s; expected listeners: %v", body, expected)
	}
}

// reserveFirewallPort holds an ephemeral wildcard TCP port until the caller releases the listener.
func reserveFirewallPort(t *testing.T) (net.Listener, int) {
	t.Helper()
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener, listener.Addr().(*net.TCPAddr).Port
}

// TestFirewallLifecycle checks default-disabled copies, ACL/port changes, loopback, stop and delete.
func TestFirewallLifecycle(t *testing.T) {
	t.Setenv("RELAY_NETWORK_MODE", "host")
	app, cookie, csrf, input := copyAPIFixture(t)
	assertFirewall(t, app)
	save := func(method, path string) MappingView { return saveCopyAPI(t, app, cookie, csrf, method, path, input) }
	input.ListenHost, input.Enabled = "0.0.0.0", true
	input.AllowedCIDRs = []string{"192.168.10.0/24"}
	original := save("POST", "/api/mappings")
	path := "/api/mappings/" + original.ID
	expected := firewallListener{input.ListenHost, input.ListenPort, input.AllowedCIDRs}
	assertFirewall(t, app, expected)
	var copiedBody map[string]any
	if err := json.Unmarshal([]byte(copyJSON(t, input)), &copiedBody); err != nil {
		t.Fatal(err)
	}
	delete(copiedBody, "enabled")
	if response := requestAPI(app, "POST", "/api/mappings", copyJSON(t, copiedBody), cookie, csrf); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	assertFirewall(t, app, expected)
	input.AllowedCIDRs = []string{"10.10.0.0/16", "192.168.1.3/32"}
	save("PUT", path)
	expected.Sources = input.AllowedCIDRs
	assertFirewall(t, app, expected)
	reserved, port := reserveFirewallPort(t)
	if err := reserved.Close(); err != nil {
		t.Fatal(err)
	}
	input.ListenPort = port
	save("PUT", path)
	expected.Port = port
	assertFirewall(t, app, expected)
	input.ListenHost = "127.0.0.1"
	save("PUT", path)
	assertFirewall(t, app)
	input.ListenHost = "0.0.0.0"
	save("PUT", path)
	input.Enabled = false
	save("PUT", path)
	assertFirewall(t, app)
	input.Enabled = true
	save("PUT", path)
	assertFirewall(t, app, expected)
	if response := requestAPI(app, "DELETE", path, "", cookie, csrf); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	assertFirewall(t, app)
}

// TestFirewallRecovery checks bind failure, proxy-triggered rebind, explicit retry and restart.
func TestFirewallRecovery(t *testing.T) {
	t.Setenv("RELAY_NETWORK_MODE", "host")
	app, cookie, csrf, input := copyAPIFixture(t)
	reserved, port := reserveFirewallPort(t)
	input.ListenHost, input.ListenPort, input.Enabled = "0.0.0.0", port, true
	initial := saveCopyAPI(t, app, cookie, csrf, "POST", "/api/mappings", input)
	if initial.Status != "error" {
		t.Fatal("busy listener not reported")
	}
	assertFirewall(t, app)
	if err := reserved.Close(); err != nil {
		t.Fatal(err)
	}
	proxy := `{"name":"changed","host":"127.0.0.1","port":1081,"username":"private","password":"not-in-snapshot"}`
	if response := requestAPI(app, "PUT", "/api/proxies/"+input.ProxyID, proxy, cookie, csrf); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	expected := firewallListener{input.ListenHost, input.ListenPort, input.AllowedCIDRs}
	assertFirewall(t, app, expected)
	// Simulate a fatal Accept error, not a configuration mutation.
	if err := app.runtimes[initial.ID].listener.Close(); err != nil {
		t.Fatal(err)
	}
	waitFirewall(t, app, 0)
	saveCopyAPI(t, app, cookie, csrf, "PUT", "/api/mappings/"+initial.ID, input)
	assertFirewall(t, app, expected)
	app.Close()
	assertFirewall(t, app)
	restarted, err := newApp(app.store, "test-password-long", app.apiAddr, "", 32)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	app.Close() // A duplicate Close from the old generation must not publish again.
	assertFirewall(t, restarted, expected)
}

// waitFirewall allows the asynchronous error/retry callback to publish a complete snapshot.
func waitFirewall(t *testing.T, app *App, count int) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		body, err := os.ReadFile(filepath.Join(app.store.directory, firewallFilename))
		var snapshot firewallSnapshot
		if err == nil && json.Unmarshal(body, &snapshot) == nil && snapshot.Version == 1 && len(snapshot.Listeners) == count {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("firewall synchronization timed out")
}

// TestFirewallFailureAndRetry verifies partial commits are explicit and never follow a symlink.
func TestFirewallFailureAndRetry(t *testing.T) {
	t.Setenv("RELAY_NETWORK_MODE", "host")
	app, cookie, csrf, input := copyAPIFixture(t)
	path := filepath.Join(app.store.directory, firewallFilename)
	external := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(external, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, path); err != nil {
		t.Fatal(err)
	}
	input.ListenHost, input.Enabled = "0.0.0.0", true
	response := requestAPI(app, "POST", "/api/mappings", copyJSON(t, input), cookie, csrf)
	if response.Code != 500 || !strings.Contains(response.Body.String(), "configuration saved") || !strings.Contains(response.Body.String(), "firewall") {
		t.Fatal(response.Code, response.Body.String())
	}
	if data, err := os.ReadFile(external); err != nil || string(data) != "untouched" {
		t.Fatal("symlink target changed", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	waitFirewall(t, app, 1)
	assertFirewall(t, app, firewallListener{input.ListenHost, input.ListenPort, input.AllowedCIDRs})
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := app.shutdown(); err == nil {
		t.Fatal("shutdown hid snapshot failure")
	}
	if restarted, err := newApp(app.store, "test-password-long", app.apiAddr, "", 32); err == nil {
		restarted.Close()
		t.Fatal("startup accepted invalid destination")
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort(input.ListenHost, strconv.Itoa(input.ListenPort)))
	if err != nil {
		t.Fatal("failed startup leaked its listener:", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestFirewallCloseVsSave verifies serialization forbids resurrecting a listener after shutdown.
func TestFirewallCloseVsSave(t *testing.T) {
	t.Setenv("RELAY_NETWORK_MODE", "host")
	app, cookie, csrf, input := copyAPIFixture(t)
	input.ListenHost, input.Enabled = "0.0.0.0", true
	body := copyJSON(t, input)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for index := 0; index < 8; index++ {
		workers.Add(1)
		go func() { defer workers.Done(); <-start; requestAPI(app, "POST", "/api/mappings", body, cookie, csrf) }()
	}
	close(start)
	app.Close()
	workers.Wait()
	if response := requestAPI(app, "POST", "/api/mappings", body, cookie, csrf); response.Code == 200 {
		t.Fatal("commit succeeded after close")
	}
	assertFirewall(t, app)
}

// TestFirewallStartupRevokesStaleState simulates a crash followed by unreadable encrypted config.
func TestFirewallStartupRevokesStaleState(t *testing.T) {
	t.Setenv("RELAY_NETWORK_MODE", "host")
	store, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stale := firewallSnapshot{1, []firewallListener{{"0.0.0.0", 33063, []string{"192.168.10.0/24"}}}}
	if err := store.writeFirewall(stale); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.directory, "config.enc"), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if app, err := newApp(store, "test-password-long", "127.0.0.1:8080", "", 32); err == nil {
		app.Close()
		t.Fatal("invalid configuration accepted")
	}
	assertFirewall(t, &App{store: store})
}

// TestFirewallAtomicReplacement verifies open readers retain an intact old generation and no temp file leaks.
func TestFirewallAtomicReplacement(t *testing.T) {
	t.Setenv("RELAY_NETWORK_MODE", "host")
	app, cookie, csrf, input := copyAPIFixture(t)
	path := filepath.Join(app.store.directory, firewallFilename)
	before, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer before.Close()
	input.ListenHost, input.Enabled = "0.0.0.0", true
	saveCopyAPI(t, app, cookie, csrf, "POST", "/api/mappings", input)
	var old firewallSnapshot
	if err := json.NewDecoder(before).Decode(&old); err != nil || old.Listeners == nil || len(old.Listeners) != 0 {
		t.Fatal("open snapshot changed across publication", old, err)
	}
	assertFirewall(t, app, firewallListener{input.ListenHost, input.ListenPort, input.AllowedCIDRs})
	if names, err := filepath.Glob(filepath.Join(app.store.directory, ".firewall-listeners-*")); err != nil || len(names) != 0 {
		t.Fatal("temporary file leaked", names, err)
	}
}

// TestFirewallBridgeIsolation proves a bridge lifecycle never creates or rewrites the host file.
func TestFirewallBridgeIsolation(t *testing.T) {
	t.Setenv("RELAY_NETWORK_MODE", "bridge")
	app, cookie, csrf, input := copyAPIFixture(t)
	path := filepath.Join(app.store.directory, firewallFilename)
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("bridge created snapshot", err)
	}
	if err := os.WriteFile(path, []byte("sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	input.Enabled = true
	saveCopyAPI(t, app, cookie, csrf, "POST", "/api/mappings", input)
	app.Close()
	if data, err := os.ReadFile(path); err != nil || string(data) != "sentinel" {
		t.Fatal("bridge modified snapshot", err)
	}
}
