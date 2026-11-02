package main

import (
	"context"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	redisProbeTimeout = 3 * time.Second
	redisCacheTTL     = 5 * time.Second
	redisResponseMax  = 1 << 20
)

// redisStatus is the public, credential-free snapshot; nil pointers encode missing values as null.
type redisStatus struct {
	Configured bool            `json:"configured"`
	Address    string          `json:"address"`
	State      string          `json:"state"`
	CheckedAt  *string         `json:"checkedAt"`
	LatencyMs  *float64        `json:"latencyMs"`
	Error      string          `json:"error"`
	Metrics    *redisMetrics   `json:"metrics"`
	Databases  []redisDatabase `json:"databases"`
}

// redisMetrics contains only the explicitly approved INFO fields.
type redisMetrics struct {
	Version                 *string `json:"version"`
	UptimeSeconds           *int64  `json:"uptimeSeconds"`
	UsedMemoryBytes         *int64  `json:"usedMemoryBytes"`
	MaxMemoryBytes          *int64  `json:"maxMemoryBytes"`
	ConnectedClients        *int64  `json:"connectedClients"`
	OpsPerSecond            *int64  `json:"opsPerSecond"`
	TotalKeys               *int64  `json:"totalKeys"`
	RDBLastSaveTime         *int64  `json:"rdbLastSaveTime"`
	RDBLastBgsaveStatus     *string `json:"rdbLastBgsaveStatus"`
	RDBBgsaveInProgress     *bool   `json:"rdbBgsaveInProgress"`
	RDBChangesSinceLastSave *int64  `json:"rdbChangesSinceLastSave"`
	AOFEnabled              *bool   `json:"aofEnabled"`
}

// redisDatabase exposes key counts only, never key names or values.
type redisDatabase struct {
	Name    string `json:"name"`
	Keys    int64  `json:"keys"`
	Expires int64  `json:"expires"`
}

// redisMonitor owns a process-wide cache and one shared probe, independently of app.mu.
type redisMonitor struct {
	mu         sync.Mutex
	base       redisStatus
	cached     redisStatus
	expires    time.Time
	flight     chan struct{}
	ctx        context.Context
	cancel     context.CancelFunc
	slots      chan struct{}
	generation uint64
	clearing   bool
	lastClear  time.Time
}

// newRedisMonitor validates the fixed environment address without dialing or failing app startup.
// Invalid input is not reflected because it might accidentally contain credentials.
func newRedisMonitor(address string) *redisMonitor {
	ctx, cancel := context.WithCancel(context.Background())
	monitor := &redisMonitor{ctx: ctx, cancel: cancel, slots: make(chan struct{}, 4)}
	monitor.base = redisStatus{State: "unconfigured", Databases: []redisDatabase{}}
	address = strings.TrimSpace(address)
	if address == "" {
		return monitor
	}
	monitor.base.Configured = true
	monitor.base.State = "error"
	host, portText, err := net.SplitHostPort(address)
	port, portErr := strconv.Atoi(portText)
	if err != nil || !validHost(host) || portErr != nil || port < 1 || port > 65535 || strings.Trim(portText, "0123456789") != "" {
		monitor.base.Error = "Redis 地址配置无效，请使用 host:port"
		return monitor
	}
	monitor.base.Address = net.JoinHostPort(host, strconv.Itoa(port))
	return monitor
}

// snapshot returns the shared five-second result; caller cancellation never cancels other sessions' probe.
// The first caller performs I/O without any application or monitor lock held.
func (monitor *redisMonitor) snapshot(requestContext context.Context) redisStatus {
	requestContext, cancel := context.WithTimeout(requestContext, redisProbeTimeout)
	defer cancel()
	for {
		if requestContext.Err() != nil {
			return redisFailure(monitor.base, "error", "Redis 状态请求已取消或超时")
		}
		monitor.mu.Lock()
		if monitor.ctx.Err() != nil {
			monitor.mu.Unlock()
			return redisFailure(monitor.base, "error", "Redis 监控已停止")
		}
		if !monitor.base.Configured || monitor.base.Address == "" {
			monitor.mu.Unlock()
			return monitor.base
		}
		if time.Now().Before(monitor.expires) {
			result := monitor.cached
			monitor.mu.Unlock()
			return result
		}
		if monitor.flight != nil {
			flight := monitor.flight
			monitor.mu.Unlock()
			select {
			case <-flight:
				continue
			case <-requestContext.Done():
				return redisFailure(monitor.base, "error", "Redis 状态请求已取消")
			}
		}
		monitor.flight = make(chan struct{})
		generation := monitor.generation
		monitor.mu.Unlock()
		deadline, _ := requestContext.Deadline()
		probeContext, stopProbe := context.WithDeadline(monitor.ctx, deadline)
		result := monitor.probe(probeContext)
		stopProbe()
		monitor.mu.Lock()
		if generation == monitor.generation && !monitor.clearing {
			monitor.cached = result
			monitor.expires = time.Now().Add(redisCacheTTL)
		}
		close(monitor.flight)
		monitor.flight = nil
		monitor.mu.Unlock()
		if result.State != "online" {
			log.Printf("Redis monitor probe failed: %s", result.Error)
		}
		return result
	}
}

// close cancels pending network work and waits outside locks; it is safe to call repeatedly.
func (monitor *redisMonitor) close() {
	monitor.cancel()
	monitor.mu.Lock()
	flight := monitor.flight
	monitor.mu.Unlock()
	if flight != nil {
		<-flight
	}
}

// redisFailure returns a safe business error without response bodies, endpoints or credentials.
func redisFailure(base redisStatus, state, message string) redisStatus {
	base.State, base.Error = state, message
	base.Metrics, base.LatencyMs = nil, nil
	base.Databases = []redisDatabase{}
	return base
}
