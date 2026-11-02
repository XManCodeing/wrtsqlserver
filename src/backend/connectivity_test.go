package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"testing"
	"time"
)

// TestConnectivityRetention checks exact two-hour pruning, bucket replacement and ordering.
func TestConnectivityRetention(t *testing.T) {
	now := time.Unix(1800000030, 0)
	points := []ConnectivityPoint{}
	for minute := 140; minute >= 0; minute-- {
		point := ConnectivityPoint{CheckedAt: now.Add(-time.Duration(minute) * time.Minute).UnixMilli(), OK: true}
		points = upsertConnectivity(points, point, now)
	}
	if len(points) != 121 || points[0].CheckedAt != now.Add(-2*time.Hour).UnixMilli() {
		t.Fatalf("incorrect inclusive retention: %d points", len(points))
	}
	latest := ConnectivityPoint{CheckedAt: now.Add(time.Second).UnixMilli(), Error: "unavailable", LatencyMs: 12}
	points = upsertConnectivity(points, latest, now.Add(time.Second))
	points = upsertConnectivity(points, ConnectivityPoint{CheckedAt: now.UnixMilli(), OK: true}, now.Add(time.Second))
	if len(points) != 120 || points[len(points)-1] != latest {
		t.Fatal("latest minute sample was not retained or exact cutoff was ignored")
	}
	points = upsertConnectivity(points, ConnectivityPoint{CheckedAt: now.Add(-3 * time.Hour).UnixMilli()}, now)
	if len(points) != 120 {
		t.Fatal("expired sample was accepted")
	}
	if remaining := pruneConnectivity(points, now.Add(3*time.Hour)); len(remaining) != 0 {
		t.Fatal("expired history remains")
	}
}

// installConnectivityMapping adds one test runtime under the same lock used by CRUD.
// enabled chooses listener state; returned identity allows generation rejection assertions.
func installConnectivityMapping(t *testing.T, app *App, proxy Proxy, enabled bool) *Runtime {
	t.Helper()
	mapping := Mapping{ID: "history-mapping", Name: "history", ListenHost: "127.0.0.1", TargetHost: "db.internal", TargetPort: 5432, ProxyID: proxy.ID, Enabled: enabled, MaxConnections: 8, AllowedCIDRs: []string{"127.0.0.1/32"}}
	app.mu.Lock()
	defer app.mu.Unlock()
	previous := app.cfg
	app.cfg.Proxies = []Proxy{proxy}
	app.cfg.Mappings = []Mapping{mapping}
	app.reconcile(previous)
	return app.runtimes[mapping.ID]
}

// waitConnectivity waits for one completed sample with a bounded, non-busy polling loop.
// It returns the detached history for id or fails the calling test after two seconds.
func waitConnectivity(t *testing.T, app *App, id string) []ConnectivityPoint {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		points := app.connectivitySnapshot(time.Now()).Histories[id]
		if len(points) > 0 {
			return points
		}
		select {
		case <-deadline.C:
			t.Fatal("connectivity sample did not arrive")
		case <-ticker.C:
		}
	}
}

// TestConnectivityBackgroundAndDisabled verifies real automatic samples and disabled retention.
func TestConnectivityBackgroundAndDisabled(t *testing.T) {
	proxy, names := fakeSOCKS(t, "", "")
	app := testApp(t)
	runtime := installConnectivityMapping(t, app, proxy, true)
	app.enqueueConnectivity()
	points := waitConnectivity(t, app, runtime.mapping.ID)
	if len(points) != 1 || !points[0].OK || !points[0].Proxy.OK || points[0].LatencyMs < points[0].Proxy.LatencyMs || points[0].CheckedAt < app.started.UnixMilli() {
		t.Fatal("unexpected initial history", points)
	}
	select {
	case target := <-names:
		if target != "db.internal:5432" {
			t.Fatal("incorrect remote SOCKS target", target)
		}
	case <-time.After(time.Second):
		t.Fatal("SOCKS CONNECT was not observed")
	}
	app.mu.Lock()
	previous := cloneConfig(app.cfg)
	app.cfg.Mappings[0].Enabled = false
	app.reconcile(previous)
	disabled := app.runtimes[runtime.mapping.ID]
	app.mu.Unlock()
	app.enqueueConnectivity()
	app.runConnectivityJob(disabled)
	if history := app.connectivitySnapshot(time.Now()).Histories[runtime.mapping.ID]; len(history) != 1 || history[0] != points[0] {
		t.Fatal("disable discarded or changed history", history)
	}
	select {
	case <-names:
		t.Fatal("disabled mapping was automatically probed")
	default:
	}
}

// TestConnectivityEditsRejectLateResults verifies generation guards, reset and delete cleanup.
func TestConnectivityEditsRejectLateResults(t *testing.T) {
	app := testApp(t)
	proxy := Proxy{ID: "proxy", Host: "127.0.0.1", Port: 1}
	runtime := installConnectivityMapping(t, app, proxy, false)
	point := ConnectivityPoint{CheckedAt: time.Now().UnixMilli(), OK: true}
	app.recordConnectivity(runtime, point)
	app.mu.Lock()
	previous := cloneConfig(app.cfg)
	app.cfg.Mappings[0].TargetHost = "changed.internal"
	app.reconcile(previous)
	replacement := app.runtimes[runtime.mapping.ID]
	app.mu.Unlock()
	app.recordConnectivity(runtime, point)
	if points := app.connectivitySnapshot(time.Now()).Histories[runtime.mapping.ID]; len(points) != 0 {
		t.Fatal("old-generation probe repopulated edited mapping", points)
	}
	app.recordConnectivity(replacement, point)
	app.mu.Lock()
	previous = cloneConfig(app.cfg)
	app.cfg.Proxies[0].Password = "changed-test-password"
	app.reconcile(previous)
	app.mu.Unlock()
	app.recordConnectivity(replacement, point)
	if points := app.connectivitySnapshot(time.Now()).Histories[runtime.mapping.ID]; len(points) != 0 {
		t.Fatal("proxy edit did not reset or reject late history")
	}
	app.mu.Lock()
	previous = cloneConfig(app.cfg)
	app.cfg.Mappings = nil
	app.reconcile(previous)
	app.mu.Unlock()
	app.recordConnectivity(runtime, point)
	if len(app.connectivitySnapshot(time.Now()).Histories) != 0 {
		t.Fatal("deleted mapping remains in history API")
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if len(app.histories) != 0 {
		t.Fatal("deleted mapping remains in history storage")
	}
}

// TestConnectivityAPIAuthAndManual verifies session/origin authorization and manual recording.
func TestConnectivityAPIAuthAndManual(t *testing.T) {
	proxy, _ := fakeSOCKS(t, "", "")
	app := testApp(t)
	runtime := installConnectivityMapping(t, app, proxy, false)
	if response := requestAPI(app, "GET", "/api/connectivity", "", nil, ""); response.Code != 401 {
		t.Fatal("anonymous history access", response.Code)
	}
	cookie, csrf := loginTest(t, app)
	response := requestAPI(app, "GET", "/api/connectivity", "", cookie, "")
	var snapshot ConnectivityResponse
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &snapshot) != nil {
		t.Fatal("authenticated history failed", response.Body.String())
	}
	if snapshot.ServerTime < app.started.UnixMilli() || snapshot.IntervalSeconds != 60 || snapshot.WindowSeconds != 7200 || snapshot.Histories[runtime.mapping.ID] == nil {
		t.Fatal("incorrect contract or null empty history", response.Body.String())
	}
	request := httptest.NewRequest("GET", "http://localhost/api/connectivity", nil)
	request.AddCookie(cookie)
	request.Header.Set("Origin", "https://evil.example")
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, request)
	if recorder.Code != 403 {
		t.Fatal("cross-origin history accepted", recorder.Code)
	}
	for attempt := 0; attempt < 2; attempt++ {
		response = requestAPI(app, "POST", "/api/mappings/"+runtime.mapping.ID+"/test", "", cookie, csrf)
		if response.Code != 200 {
			t.Fatal("manual probe failed", response.Body.String())
		}
		var point ConnectivityPoint
		if err := json.Unmarshal(response.Body.Bytes(), &point); err != nil {
			t.Fatal(err)
		}
		if !point.OK || !point.Proxy.OK || point.LatencyMs < point.Proxy.LatencyMs {
			t.Fatal("manual probe missing proxy metrics", response.Body.String())
		}
	}
	points := app.connectivitySnapshot(time.Now()).Histories[runtime.mapping.ID]
	if len(points) < 1 || len(points) > 2 || !points[len(points)-1].OK || !points[len(points)-1].Proxy.OK {
		t.Fatal("manual result not recorded", points)
	}
	points[0].Error = "mutated external snapshot"
	if app.connectivitySnapshot(time.Now()).Histories[runtime.mapping.ID][0].Error != "" {
		t.Fatal("snapshot aliases live history")
	}
}

// TestConnectivityQueueBounded ensures hundreds of mappings use a bounded deduplicated queue.
func TestConnectivityQueueBounded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	app := &App{runtimes: map[string]*Runtime{}, histories: map[string][]ConnectivityPoint{}, probePending: map[*Runtime]bool{}, probeJobs: make(chan *Runtime, connectivityQueueSize)}
	for index := 0; index < 256; index++ {
		id := fmt.Sprintf("mapping-%d", index)
		app.runtimes[id] = &Runtime{mapping: Mapping{ID: id, Enabled: true}, ctx: ctx}
	}
	app.enqueueConnectivity()
	app.enqueueConnectivity()
	if len(app.probeJobs) != 256 || len(app.probePending) != 256 {
		t.Fatal("queue overflowed or duplicated jobs")
	}
}

// silentConnectivityProxy accepts one connection but never answers the SOCKS handshake.
// It returns the endpoint and acceptance channel; cleanup releases the listener and peer.
func silentConnectivityProxy(t *testing.T) (Proxy, <-chan net.Conn) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		connection, err := listener.Accept()
		if err == nil {
			accepted <- connection
		}
	}()
	t.Cleanup(func() { listener.Close(); <-done })
	return Proxy{ID: "silent", Host: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port}, accepted
}

// TestConnectivityCloseCancelsHandshake verifies Close interrupts probes without app.mu deadlock.
func TestConnectivityCloseCancelsHandshake(t *testing.T) {
	proxy, accepted := silentConnectivityProxy(t)
	app := testApp(t)
	runtime := installConnectivityMapping(t, app, proxy, true)
	app.enqueueConnectivity()
	select {
	case connection := <-accepted:
		defer connection.Close()
	case <-time.After(time.Second):
		t.Fatal("probe did not start")
	}
	closed := make(chan struct{})
	go func() { app.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel SOCKS handshake or deadlocked")
	}
	if len(app.connectivitySnapshot(time.Now()).Histories[runtime.mapping.ID]) != 0 || len(app.testSlots) != 0 {
		t.Fatal("canceled probe recorded downtime or leaked a slot")
	}
}

// TestConnectivityDisableCancelsActiveProbe verifies edits interrupt an actual stalled handshake.
func TestConnectivityDisableCancelsActiveProbe(t *testing.T) {
	proxy, accepted := silentConnectivityProxy(t)
	app := testApp(t)
	runtime := installConnectivityMapping(t, app, proxy, true)
	point := ConnectivityPoint{CheckedAt: time.Now().UnixMilli(), OK: true}
	app.recordConnectivity(runtime, point)
	app.enqueueConnectivity()
	select {
	case connection := <-accepted:
		defer connection.Close()
	case <-time.After(time.Second):
		t.Fatal("probe did not start")
	}
	disabled := make(chan struct{})
	go func() {
		app.mu.Lock()
		previous := cloneConfig(app.cfg)
		app.cfg.Mappings[0].Enabled = false
		app.reconcile(previous)
		app.mu.Unlock()
		close(disabled)
	}()
	select {
	case <-disabled:
	case <-time.After(time.Second):
		t.Fatal("disable blocked on an active probe")
	}
	app.Close()
	points := app.connectivitySnapshot(time.Now()).Histories[runtime.mapping.ID]
	if len(points) != 1 || points[0] != point || len(app.testSlots) != 0 {
		t.Fatal("canceled probe replaced retained history or leaked a slot")
	}
}

// TestConnectivityStartupAndRestart verifies lifecycle startup and intentionally volatile history.
func TestConnectivityStartupAndRestart(t *testing.T) {
	proxy, _ := fakeSOCKS(t, "", "")
	app := testApp(t)
	installConnectivityMapping(t, app, proxy, true)
	app.mu.Lock()
	// Keep the real listener bound: the restarted runtime may report a bind error,
	// but upstream availability must still be independently checked through SOCKS.
	app.cfg.Mappings[0].ListenPort = app.runtimes["history-mapping"].listener.Addr().(*net.TCPAddr).Port
	err := app.store.Save(app.cfg)
	app.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := newApp(app.store, "test-password-long", "127.0.0.1:8080", "", 32)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	points := waitConnectivity(t, restarted, "history-mapping")
	if len(points) != 1 || !points[0].OK || points[0].CheckedAt < restarted.started.UnixMilli() {
		t.Fatal("startup did not probe or fabricated earlier history", points)
	}
	restarted.mu.Lock()
	candidate := cloneConfig(restarted.cfg)
	candidate.Mappings[0].Enabled = false
	err = restarted.commit(candidate)
	restarted.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	restarted.Close()
	empty, err := newApp(app.store, "test-password-long", "127.0.0.1:8080", "", 32)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(empty.Close)
	if len(empty.connectivitySnapshot(time.Now()).Histories["history-mapping"]) != 0 {
		t.Fatal("history unexpectedly survived process lifecycle restart")
	}
}

// TestConnectivitySharedSlots verifies background probes reserve the same slots as manual tests.
func TestConnectivitySharedSlots(t *testing.T) {
	proxy, accepted := silentConnectivityProxy(t)
	app := testApp(t)
	runtime := installConnectivityMapping(t, app, proxy, true)
	for slot := 0; slot < 3; slot++ {
		app.testSlots <- struct{}{}
	}
	defer func() {
		for slot := 0; slot < 3; slot++ {
			<-app.testSlots
		}
	}()
	app.enqueueConnectivity()
	select {
	case connection := <-accepted:
		defer connection.Close()
	case <-time.After(time.Second):
		t.Fatal("background probe did not consume the remaining shared slot")
	}
	cookie, csrf := loginTest(t, app)
	response := requestAPI(app, "POST", "/api/mappings/"+runtime.mapping.ID+"/test", "", cookie, csrf)
	if response.Code != 429 || len(app.testSlots) != 4 {
		t.Fatal("manual test bypassed shared concurrency limit", response.Code)
	}
	app.Close()
}

// TestConnectivityCloseCancelsSlotWait verifies workers blocked by manual slots exit promptly.
func TestConnectivityCloseCancelsSlotWait(t *testing.T) {
	app := testApp(t)
	installConnectivityMapping(t, app, Proxy{ID: "proxy", Host: "127.0.0.1", Port: 1}, true)
	for slot := 0; slot < cap(app.testSlots); slot++ {
		app.testSlots <- struct{}{}
	}
	app.enqueueConnectivity()
	closed := make(chan struct{})
	go func() { app.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close blocked waiting for occupied manual test slots")
	}
	for slot := 0; slot < cap(app.testSlots); slot++ {
		<-app.testSlots
	}
}
