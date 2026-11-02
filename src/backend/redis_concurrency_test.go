package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestRedisSharedConcurrencyLimit verifies four blocked reads share the same slots as status and clear.
// Extra work returns immediately instead of queueing or opening an unbounded connection.
func TestRedisSharedConcurrencyLimit(t *testing.T) {
	entered, release := make(chan struct{}, 4), make(chan struct{})
	address, _ := fakeRedis(t, func(connection net.Conn) {
		reader := bufio.NewReader(connection)
		if !expectRedisCommand(t, reader, string(redisCommandBytes("SELECT", "0"))) {
			return
		}
		entered <- struct{}{}
		<-release
		if err := writeAll(connection, []byte("+OK\r\n")); err != nil {
			t.Error(err)
			return
		}
		if !expectRedisCommand(t, reader, string(redisCommandBytes("SCAN", "0", "MATCH", "*", "COUNT", "50"))) {
			return
		}
		if err := writeAll(connection, []byte("*2\r\n"+redisBulk("0")+redisArray())); err != nil {
			t.Error(err)
		}
	})
	monitor := newRedisMonitor(address)
	defer monitor.close()
	var callers sync.WaitGroup
	for index := 0; index < 4; index++ {
		callers.Add(1)
		go func() {
			defer callers.Done()
			request := httptest.NewRequest("GET", "http://localhost/?cursor=0", nil)
			if _, err := monitor.scanKeys(request, "db0"); err != nil {
				t.Error(err)
			}
		}()
	}
	for index := 0; index < 4; index++ {
		<-entered
	}
	request := httptest.NewRequest("GET", "http://localhost/", nil)
	if _, err := monitor.scanKeys(request, "db0"); err != errRedisBusy {
		t.Error("fifth scan was not rejected", err)
	}
	if status := monitor.snapshot(context.Background()); status.State == "online" {
		t.Error("status bypassed shared limit")
	}
	recorder := httptest.NewRecorder()
	request = httptest.NewRequest("POST", "http://localhost/", strings.NewReader(fmt.Sprintf(`{"confirmation":"清空 db0","address":%q}`, address)))
	request.Header.Set("Content-Type", "application/json")
	monitor.clearDatabase(recorder, request, "db0")
	if recorder.Code != 429 {
		t.Error("clear bypassed shared limit", recorder.Code)
	}
	close(release)
	callers.Wait()
}

// TestRedisClearNonblockingMutex rejects a second clear even after its cooldown expires while one is pending.
func TestRedisClearNonblockingMutex(t *testing.T) {
	monitor := newRedisMonitor("127.0.0.1:1")
	defer monitor.close()
	if !monitor.beginClear() {
		t.Fatal("first clear denied")
	}
	monitor.mu.Lock()
	monitor.lastClear = time.Now().Add(-time.Minute)
	monitor.mu.Unlock()
	if monitor.beginClear() {
		t.Fatal("concurrent clear accepted")
	}
	monitor.endClear()
	if !monitor.beginClear() {
		t.Fatal("clear mutex not released")
	}
	monitor.endClear()
}

// TestRedisClearInvalidatesInflightCache prevents a pre-clear INFO snapshot from being cached afterward.
// A second status read must open a new connection; the first caller may receive its own older observation.
func TestRedisClearInvalidatesInflightCache(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var first sync.Once
	fixture := redisFixture(t, "+PONG\r\n", redisBulk(redisTestInfo))
	address, connections := fakeRedis(t, func(connection net.Conn) {
		first.Do(func() { close(entered); <-release })
		fixture(connection)
	})
	monitor := newRedisMonitor(address)
	defer monitor.close()
	done := make(chan redisStatus, 1)
	go func() { done <- monitor.snapshot(context.Background()) }()
	<-entered
	if !monitor.beginClear() {
		t.Fatal("clear lock unavailable")
	}
	monitor.endClear()
	close(release)
	if status := <-done; status.State != "online" {
		t.Fatal(status.State)
	}
	monitor.mu.Lock()
	invalid := monitor.expires.IsZero()
	monitor.mu.Unlock()
	if !invalid {
		t.Fatal("stale in-flight status repopulated cache")
	}
	if status := monitor.snapshot(context.Background()); status.State != "online" || connections.Load() != 2 {
		t.Fatal("status did not refresh")
	}
}
