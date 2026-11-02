package main

import (
	"context"
	"log"
	"net/http"
	"reflect"
	"sort"
	"time"
)

const (
	connectivityInterval  = time.Minute
	connectivityWindow    = 2 * time.Hour
	connectivityWorkers   = 4
	connectivityQueueSize = 256
)

// ProxyConnectivity describes TCP connection and SOCKS authentication, before target CONNECT.
// LatencyMs includes endpoint dialing and authentication; Error never contains credentials.
type ProxyConnectivity struct {
	OK        bool   `json:"ok"`
	LatencyMs int64  `json:"latencyMs"`
	Error     string `json:"error,omitempty"`
}

// ConnectivityPoint describes one completed SOCKS CONNECT, not database authentication.
// CheckedAt is Unix milliseconds; top-level fields continue to measure the complete target path.
type ConnectivityPoint struct {
	CheckedAt int64             `json:"checkedAt"`
	OK        bool              `json:"ok"`
	LatencyMs int64             `json:"latencyMs"`
	Error     string            `json:"error,omitempty"`
	Proxy     ProxyConnectivity `json:"proxy"`
}

// ConnectivityResponse contains actual samples only; missing minutes remain absent.
type ConnectivityResponse struct {
	ServerTime      int64                          `json:"serverTime"`
	IntervalSeconds int                            `json:"intervalSeconds"`
	WindowSeconds   int                            `json:"windowSeconds"`
	Histories       map[string][]ConnectivityPoint `json:"histories"`
}

// connectivityChanged returns whether an edit resets history; enable-only changes retain it.
// Both mappings and proxies are immutable copies owned by their runtime generation.
func connectivityChanged(previous, next Mapping, oldProxy, newProxy Proxy) bool {
	previous.Enabled = next.Enabled
	return !reflect.DeepEqual(previous, next) || !reflect.DeepEqual(oldProxy, newProxy)
}

// startConnectivity initializes memory-only history and a fixed worker pool for app lifetime.
// Initial samples start now, never backfilled; restarting the process clears every sample.
func (app *App) startConnectivity() {
	app.histories = make(map[string][]ConnectivityPoint)
	app.probePending = make(map[*Runtime]bool)
	app.probeJobs = make(chan *Runtime, connectivityQueueSize)
	app.probeCtx, app.probeCancel = context.WithCancel(context.Background())
	app.enqueueConnectivity()
	app.probeWait.Add(connectivityWorkers + 1)
	for worker := 0; worker < connectivityWorkers; worker++ {
		go app.connectivityWorker()
	}
	go app.connectivityLoop()
}

// connectivityLoop schedules enabled mappings each minute until application cancellation.
// The single ticker and bounded queue avoid creating a goroutine per mapping or tick.
func (app *App) connectivityLoop() {
	defer app.probeWait.Done()
	ticker := time.NewTicker(connectivityInterval)
	defer ticker.Stop()
	for {
		select {
		case <-app.probeCtx.Done():
			return
		case <-ticker.C:
			app.enqueueConnectivity()
		}
	}
}

// enqueueConnectivity snapshots current runtime identities without performing network I/O.
// app.mu protects the pending set; slow probes never accumulate duplicate queued jobs.
func (app *App) enqueueConnectivity() {
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.closed {
		return
	}
	now := time.Now()
	for id, runtime := range app.runtimes {
		app.histories[id] = pruneConnectivity(app.histories[id], now)
		if !runtime.mapping.Enabled || app.probePending[runtime] {
			continue
		}
		select {
		case app.probeJobs <- runtime:
			app.probePending[runtime] = true
		default:
			// A full queue is retried next tick rather than blocking configuration edits.
		}
	}
}

// connectivityWorker consumes bounded jobs and shares testSlots with manual tests.
// It never joins Runtime.wait, so reconcile can stop a runtime while holding app.mu.
func (app *App) connectivityWorker() {
	defer app.probeWait.Done()
	for {
		select {
		case <-app.probeCtx.Done():
			return
		case runtime := <-app.probeJobs:
			app.runConnectivityJob(runtime)
			app.mu.Lock()
			delete(app.probePending, runtime)
			app.mu.Unlock()
		}
	}
}

// runConnectivityJob waits cancelably for a shared slot, then probes only a live runtime.
// Cancellation from Stop or Close discards the sample instead of recording false downtime.
func (app *App) runConnectivityJob(runtime *Runtime) {
	select {
	case <-app.probeCtx.Done():
		return
	case <-runtime.ctx.Done():
		return
	case app.testSlots <- struct{}{}:
	}
	defer func() { <-app.testSlots }()
	if app.probeCtx.Err() != nil || runtime.ctx.Err() != nil || !runtime.mapping.Enabled {
		return
	}
	point := probeConnectivity(runtime.ctx, runtime)
	app.recordConnectivity(runtime, point)
}

// probeConnectivity returns one timestamped SOCKS CONNECT result within ten seconds.
// parent controls cancellation; runtime supplies the immutable endpoint and target.
func probeConnectivity(parent context.Context, runtime *Runtime) ConnectivityPoint {
	started := time.Now()
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	connection, proxy, err := dialSOCKSWithMetrics(ctx, runtime.proxy, runtime.mapping.TargetHost, runtime.mapping.TargetPort)
	finished := time.Now()
	point := ConnectivityPoint{CheckedAt: finished.UnixMilli(), OK: err == nil, LatencyMs: finished.Sub(started).Milliseconds(), Proxy: proxy}
	if err != nil {
		point.Error = err.Error()
		if parent.Err() == nil {
			log.Printf("mapping %s connectivity probe failed", runtime.mapping.ID)
		}
	} else if err := connection.Close(); err != nil {
		log.Printf("mapping %s connectivity probe connection cleanup failed", runtime.mapping.ID)
	}
	return point
}

// recordConnectivity stores a current-generation result under app.mu, never during dialing.
// Results from an edited, deleted or stopped runtime cannot repopulate history.
// Explicit manual tests may still record samples for a currently disabled mapping.
func (app *App) recordConnectivity(runtime *Runtime, point ConnectivityPoint) {
	app.mu.Lock()
	defer app.mu.Unlock()
	id := runtime.mapping.ID
	if app.closed || app.runtimes[id] != runtime || runtime.ctx.Err() != nil {
		return
	}
	app.histories[id] = upsertConnectivity(app.histories[id], point, time.Now())
}

// pruneConnectivity returns only real timestamps in the trailing two-hour window.
// Input is chronological; an inclusive cutoff can retain 121 distinct minute buckets.
func pruneConnectivity(points []ConnectivityPoint, now time.Time) []ConnectivityPoint {
	cutoff := now.Add(-connectivityWindow).UnixMilli()
	first := sort.Search(len(points), func(index int) bool { return points[index].CheckedAt >= cutoff })
	points = points[first:]
	if len(points) > 121 {
		points = points[len(points)-121:]
	}
	return points
}

// upsertConnectivity inserts point chronologically, keeping the latest sample per UTC minute.
// now supplies the real retention cutoff; older completions cannot overwrite newer samples.
func upsertConnectivity(points []ConnectivityPoint, point ConnectivityPoint, now time.Time) []ConnectivityPoint {
	points = pruneConnectivity(points, now)
	if point.CheckedAt < now.Add(-connectivityWindow).UnixMilli() {
		return points
	}
	bucket := point.CheckedAt / connectivityInterval.Milliseconds()
	index := sort.Search(len(points), func(index int) bool {
		return points[index].CheckedAt/connectivityInterval.Milliseconds() >= bucket
	})
	if index < len(points) && points[index].CheckedAt/connectivityInterval.Milliseconds() == bucket {
		if point.CheckedAt >= points[index].CheckedAt {
			points[index] = point
		}
	} else {
		points = append(points, ConnectivityPoint{})
		copy(points[index+1:], points[index:])
		points[index] = point
	}
	return pruneConnectivity(points, now)
}

// connectivitySnapshot returns detached, chronological arrays for every current mapping.
// Empty histories encode as [], and pruning also runs on reads when probes are disabled.
func (app *App) connectivitySnapshot(now time.Time) ConnectivityResponse {
	app.mu.Lock()
	defer app.mu.Unlock()
	response := ConnectivityResponse{ServerTime: now.UnixMilli(), IntervalSeconds: 60, WindowSeconds: 7200, Histories: make(map[string][]ConnectivityPoint)}
	for _, mapping := range app.cfg.Mappings {
		points := pruneConnectivity(app.histories[mapping.ID], now)
		app.histories[mapping.ID] = points
		response.Histories[mapping.ID] = append([]ConnectivityPoint{}, points...)
	}
	return response
}

// manualConnectivity links request cancellation with the runtime's edit/shutdown lifecycle.
// It returns the existing manual-test envelope and records only non-canceled real results.
func (app *App) manualConnectivity(writer http.ResponseWriter, request *http.Request, runtime *Runtime) {
	ctx, cancel := context.WithCancel(request.Context())
	stopCancel := context.AfterFunc(runtime.ctx, cancel)
	defer stopCancel()
	defer cancel()
	if runtime.ctx.Err() != nil {
		cancel()
	}
	point := probeConnectivity(ctx, runtime)
	if ctx.Err() == nil {
		app.recordConnectivity(runtime, point)
	}
	response := map[string]any{"ok": point.OK, "latencyMs": point.LatencyMs, "proxy": point.Proxy}
	if point.Error != "" {
		response["error"] = point.Error
	}
	writeJSON(writer, 200, response)
}
