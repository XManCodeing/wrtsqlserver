package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const redisTestInfo = "# Server\r\nredis_version:7.2.1\r\nuptime_in_seconds:123\r\nconfig_file:secret-path\r\n" +
	"# Memory\r\nused_memory:4096\r\nmaxmemory:0\r\n# Clients\r\nconnected_clients:2\r\n" +
	"# Stats\r\ninstantaneous_ops_per_sec:8\r\n# Persistence\r\nrdb_last_save_time:1700000000\r\n" +
	"rdb_last_bgsave_status:ok\r\nrdb_bgsave_in_progress:0\r\nrdb_changes_since_last_save:3\r\naof_enabled:1\r\n" +
	"# Keyspace\r\ndb0:keys=4,expires=2,avg_ttl=10\r\ndb2:keys=6,expires=0,avg_ttl=0\r\n"

// fakeRedis starts a loopback fixture, counts connections and runs handler for each accepted stream.
// Cleanup closes the listener and joins all bounded handlers; production services are never contacted.
func fakeRedis(t *testing.T, handler func(net.Conn)) (string, *atomic.Int64) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	connections := &atomic.Int64{}
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			connections.Add(1)
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer connection.Close()
				if deadlineErr := connection.SetDeadline(time.Now().Add(5 * time.Second)); deadlineErr != nil {
					t.Error(deadlineErr)
					return
				}
				handler(connection)
			}()
		}
	}()
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
		workers.Wait()
	})
	return listener.Addr().String(), connections
}

// expectRedisCommand reads the exact fixed RESP command; no arbitrary command parser is needed in tests.
func expectRedisCommand(t *testing.T, reader io.Reader, command string) bool {
	t.Helper()
	buffer := make([]byte, len(command))
	if _, err := io.ReadFull(reader, buffer); err != nil {
		t.Error(err)
		return false
	}
	if string(buffer) != command {
		t.Errorf("unexpected command: %q", buffer)
		return false
	}
	return true
}

// redisFixture returns a server callback that accepts only PING and INFO default and writes supplied replies.
// Extra client bytes are rejected, ensuring no AUTH, key scan or mutation commands can escape.
func redisFixture(t *testing.T, ping, info string) func(net.Conn) {
	return func(connection net.Conn) {
		reader := bufio.NewReader(connection)
		if !expectRedisCommand(t, reader, "*1\r\n$4\r\nPING\r\n") {
			return
		}
		if err := writeAll(connection, []byte(ping)); err != nil {
			t.Error(err)
			return
		}
		if ping == "+PONG\r\n" {
			if !expectRedisCommand(t, reader, "*2\r\n$4\r\nINFO\r\n$7\r\ndefault\r\n") {
				return
			}
			if err := writeAll(connection, []byte(info)); err != nil {
				t.Error(err)
				return
			}
		}
		if extra, err := reader.ReadByte(); err == nil {
			t.Errorf("unexpected command byte: %q", extra)
		}
	}
}

// redisBulk formats an INFO payload as a RESP bulk response for the fake Redis server.
func redisBulk(payload string) string {
	return fmt.Sprintf("$%d\r\n%s\r\n", len(payload), payload)
}

// readRedisStatus decodes an authenticated GET and checks the precise top-level JSON contract.
func readRedisStatus(t *testing.T, app *App) redisStatus {
	t.Helper()
	cookie, _ := loginTest(t, app)
	response := requestAPI(app, "GET", "/api/redis/status?address=evil:1&command=FLUSHALL", "", cookie, "")
	if response.Code != 200 {
		t.Fatalf("status: %d", response.Code)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 8 {
		t.Fatalf("unexpected fields: %s", response.Body.String())
	}
	var status redisStatus
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Databases == nil {
		t.Fatal("databases must be an array")
	}
	return status
}

// TestRedisStatusAuthenticationAndMetrics verifies session authorization, fixed commands and all whitelisted fields.
func TestRedisStatusAuthenticationAndMetrics(t *testing.T) {
	address, connections := fakeRedis(t, redisFixture(t, "+PONG\r\n", redisBulk(redisTestInfo)))
	t.Setenv("REDIS_ADDR", address)
	app := testApp(t)
	if response := requestAPI(app, "GET", "/api/redis/status", "", nil, ""); response.Code != 401 {
		t.Fatal(response.Code)
	}
	if connections.Load() != 0 {
		t.Fatal("unauthenticated request dialed Redis")
	}
	status := readRedisStatus(t, app)
	if status.State != "online" || !status.Configured || status.Address != address || status.Error != "" || status.LatencyMs == nil {
		t.Fatalf("invalid status: %+v", status)
	}
	if status.CheckedAt == nil {
		t.Fatal("missing checkedAt")
	}
	if _, err := time.Parse(time.RFC3339, *status.CheckedAt); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(status.Metrics)
	if err != nil {
		t.Fatal(err)
	}
	expected := `{"version":"7.2.1","uptimeSeconds":123,"usedMemoryBytes":4096,"maxMemoryBytes":0,"connectedClients":2,"opsPerSecond":8,"totalKeys":10,"rdbLastSaveTime":1700000000,"rdbLastBgsaveStatus":"ok","rdbBgsaveInProgress":false,"rdbChangesSinceLastSave":3,"aofEnabled":true}`
	if string(encoded) != expected || len(status.Databases) != 2 {
		t.Fatalf("metrics: %s; databases: %+v", encoded, status.Databases)
	}
	if strings.Contains(string(encoded), "secret-path") {
		t.Fatal("raw INFO leaked")
	}
	_ = readRedisStatus(t, app)
	if connections.Load() != 1 {
		t.Fatal("cached API request dialed again")
	}
}

// TestRedisConfiguration verifies absent/invalid configuration is a business result and never blocks startup.
func TestRedisConfiguration(t *testing.T) {
	for _, address := range []string{"", "redis://user:password@localhost:6379", "localhost", "localhost:0", "localhost:65536", "bad host:6379", "localhost:+6379"} {
		t.Run(address, func(t *testing.T) {
			t.Setenv("REDIS_ADDR", address)
			status := readRedisStatus(t, testApp(t))
			state := "error"
			if address == "" {
				state = "unconfigured"
			}
			if status.State != state || status.Configured != (address != "") || status.Address != "" || status.CheckedAt != nil || status.Metrics != nil {
				t.Fatalf("unexpected status: %+v", status)
			}
		})
	}
}

// TestRedisProtocolFailures covers authentication/ACL rejection, oversized replies and malformed RESP/INFO.
func TestRedisProtocolFailures(t *testing.T) {
	cases := []struct{ name, ping, info string }{
		{"auth", "-NOAUTH secret-auth-message\r\n", ""},
		{"info-denied", "+PONG\r\n", "-NOPERM secret-command-message\r\n"},
		{"bad-pong", "+OK\r\n", ""},
		{"nested", "*1\r\n+PONG\r\n", ""},
		{"oversized", "+PONG\r\n", "$1048576\r\n"},
		{"negative-length", "+PONG\r\n", "$-1\r\n"},
		{"bad-ending", "+PONG\r\n", "$3\r\nabcxx"},
		{"wrong-type", "+PONG\r\n", "+secret-info\r\n"},
		{"invalid-info", "+PONG\r\n", redisBulk("secret-invalid-info")},
		{"empty-info", "+PONG\r\n", redisBulk("")},
		{"invalid-number", "+PONG\r\n", redisBulk("# Memory\r\nused_memory:secret-invalid-number\r\n")},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			address, connections := fakeRedis(t, redisFixture(t, testCase.ping, testCase.info))
			t.Setenv("REDIS_ADDR", address)
			app := testApp(t)
			status := readRedisStatus(t, app)
			if status.State != "error" || status.Error == "" || status.CheckedAt == nil || status.Metrics != nil || status.LatencyMs != nil {
				t.Fatalf("unexpected error: %+v", status)
			}
			encoded, err := json.Marshal(status)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "secret-") {
				t.Fatal("raw response leaked")
			}
			_ = readRedisStatus(t, app)
			if connections.Load() != 1 {
				t.Fatal("error result was not cached")
			}
		})
	}
}

// TestRedisMissingAndKeyspace verifies missing counters remain null and an explicit empty keyspace is zero.
func TestRedisMissingAndKeyspace(t *testing.T) {
	cases := []struct {
		name, info string
		total      *int64
	}{
		{"missing", "# Server\r\nredis_version:7.0.0\r\n", nil},
		{"empty-keyspace", "# Keyspace\r\n", new(int64)},
		{"missing-keyspace", "# Clients\r\nconnected_clients:0\r\n", nil},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			address, _ := fakeRedis(t, redisFixture(t, "+PONG\r\n", redisBulk(testCase.info)))
			monitor := newRedisMonitor(address)
			defer monitor.close()
			status := monitor.snapshot(context.Background())
			if status.State != "online" || status.Metrics.UsedMemoryBytes != nil || status.Metrics.AOFEnabled != nil {
				t.Fatalf("missing fields: %+v", status)
			}
			if (status.Metrics.TotalKeys == nil) != (testCase.total == nil) {
				t.Fatal("incorrect keyspace presence")
			}
			if testCase.total != nil && *status.Metrics.TotalKeys != 0 {
				t.Fatal("empty keyspace must be zero")
			}
		})
	}
}

// TestRedisTimeoutAndShutdown checks the actual three-second timeout and cancellation without app.mu deadlocks.
func TestRedisTimeoutAndShutdown(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprint(shutdown), func(t *testing.T) {
			accepted := make(chan struct{})
			address, _ := fakeRedis(t, func(connection net.Conn) {
				close(accepted)
				_, err := io.Copy(io.Discard, connection)
				if err != nil {
					t.Error(err)
				}
			})
			t.Setenv("REDIS_ADDR", address)
			app := testApp(t)
			started := time.Now()
			done := make(chan redisStatus, 1)
			go func() { done <- app.redis.snapshot(context.Background()) }()
			<-accepted
			app.mu.Lock()
			app.mu.Unlock()
			if shutdown {
				closed := make(chan struct{})
				go func() { app.Close(); close(closed) }()
				select {
				case <-closed:
				case <-time.After(time.Second):
					t.Fatal("shutdown deadlock")
				}
			}
			select {
			case status := <-done:
				if status.State != "unreachable" || status.Metrics != nil {
					t.Fatalf("unexpected timeout state: %+v", status)
				}
			case <-time.After(4 * time.Second):
				t.Fatal("probe exceeded timeout")
			}
			if !shutdown && time.Since(started) < 2900*time.Millisecond {
				t.Fatal("timeout was not three seconds")
			}
		})
	}
}

// TestRedisSingleFlightAndCache ensures concurrent sessions share one probe and refresh exactly once after expiry.
func TestRedisSingleFlightAndCache(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var first sync.Once
	fixture := redisFixture(t, "+PONG\r\n", redisBulk(redisTestInfo))
	address, connections := fakeRedis(t, func(connection net.Conn) {
		first.Do(func() { close(entered); <-release })
		fixture(connection)
	})
	monitor := newRedisMonitor(address)
	defer monitor.close()
	var callers sync.WaitGroup
	for index := 0; index < 32; index++ {
		callers.Add(1)
		go func() {
			defer callers.Done()
			if status := monitor.snapshot(context.Background()); status.State != "online" {
				t.Errorf("concurrent snapshot: %+v", status)
			}
		}()
	}
	<-entered
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if status := monitor.snapshot(cancelled); status.State != "error" {
		t.Fatal("cancelled waiter did not leave")
	}
	close(release)
	callers.Wait()
	monitor.mu.Lock()
	remaining := time.Until(monitor.expires)
	monitor.mu.Unlock()
	if remaining < 4*time.Second || remaining > redisCacheTTL {
		t.Fatalf("wrong TTL: %s", remaining)
	}
	if status := monitor.snapshot(context.Background()); status.State != "online" || connections.Load() != 1 {
		t.Fatal("cached snapshot opened another connection")
	}
	monitor.mu.Lock()
	monitor.expires = time.Now().Add(-time.Second)
	monitor.mu.Unlock()
	if status := monitor.snapshot(context.Background()); status.State != "online" || connections.Load() != 2 {
		t.Fatal("expired snapshot did not refresh exactly once")
	}
}
