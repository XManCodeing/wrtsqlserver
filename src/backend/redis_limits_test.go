package main

import (
	"bufio"
	"context"
	"io"
	"net"
	"strings"
	"testing"
)

// TestRedisInfoValidation exercises invalid keyspace counters and duplicate/overflowed whitelisted fields.
// Every malformed case must reject the full snapshot, not quietly substitute zero values.
func TestRedisInfoValidation(t *testing.T) {
	for _, info := range []string{
		"# Keyspace\r\ndb0:keys=1\r\n",
		"# Keyspace\r\ndb0:keys=1,expires=2\r\n",
		"# Keyspace\r\ndb0:keys=1,expires=-1\r\n",
		"# Keyspace\r\ndb0:keys=1,expires=0,keys=2\r\n",
		"# Keyspace\r\ndb0:keys=9223372036854775807,expires=0\r\ndb1:keys=1,expires=0\r\n",
		"# Keyspace\r\nnotdb:keys=1,expires=0\r\n",
		"# Memory\r\nused_memory:1\r\nused_memory:2\r\n",
		"# Memory\r\nused_memory:9223372036854775808\r\n",
		"# Persistence\r\naof_enabled:2\r\n",
		"# Persistence\r\nrdb_last_bgsave_status:secret-status\r\n",
		"# Server\r\nredis_version:7.0\r\n# Server\r\nuptime_in_seconds:1\r\n",
		"# Server\nredis_version:7.0\n",
		"redis_version:7.0\r\n",
		"# Unknown\r\nvalue:1\r\n",
	} {
		if metrics, databases, err := parseRedisInfo(info); err == nil || metrics != nil || databases != nil {
			t.Errorf("accepted malformed INFO: %q", info)
		}
	}
}

// TestRedisReplyBounds verifies bounded PING lines, combined response budgets and truncated payloads.
func TestRedisReplyBounds(t *testing.T) {
	for _, testCase := range []struct {
		name, response string
		bulk           bool
		budget         int64
	}{
		{"line-limit", "+" + strings.Repeat("x", 5000) + "\r\n", false, redisResponseMax},
		{"total-budget", "$20\r\n" + strings.Repeat("x", 20) + "\r\n", true, 26},
		{"truncated", "$20\r\nshort", true, redisResponseMax},
		{"bad-simple-ending", "+PONG\n", false, redisResponseMax},
		{"overflow-length", "$99999999999999999999999\r\n", true, redisResponseMax},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			limited := &io.LimitedReader{R: strings.NewReader(testCase.response), N: testCase.budget}
			if _, err := redisReply(bufio.NewReaderSize(limited, 4096), limited, testCase.bulk); err == nil {
				t.Fatal("accepted invalid RESP")
			}
		})
	}
}

// TestRedisUnreachable returns HTTP 200 with a cached business failure for a closed loopback endpoint.
func TestRedisUnreachable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REDIS_ADDR", address)
	app := testApp(t)
	status := readRedisStatus(t, app)
	if status.State != "unreachable" || status.CheckedAt == nil || status.Error == "" || status.Metrics != nil {
		t.Fatalf("unexpected unreachable result: %+v", status)
	}
	cached := app.redis.snapshot(context.Background())
	if cached.CheckedAt != status.CheckedAt && *cached.CheckedAt != *status.CheckedAt {
		t.Fatal("unreachable result was not cached")
	}
}

// TestRedisTruncatedNetworkReply verifies premature EOF after a bulk header is a protocol business error.
func TestRedisTruncatedNetworkReply(t *testing.T) {
	address, _ := fakeRedis(t, func(connection net.Conn) {
		if !expectRedisCommand(t, connection, "*1\r\n$4\r\nPING\r\n") {
			return
		}
		if err := writeAll(connection, []byte("+PONG\r\n")); err != nil {
			t.Error(err)
			return
		}
		if !expectRedisCommand(t, connection, "*2\r\n$4\r\nINFO\r\n$7\r\ndefault\r\n") {
			return
		}
		if err := writeAll(connection, []byte("$100\r\nshort")); err != nil {
			t.Error(err)
		}
	})
	monitor := newRedisMonitor(address)
	defer monitor.close()
	if status := monitor.snapshot(context.Background()); status.State != "error" || status.Metrics != nil {
		t.Fatalf("unexpected malformed result: %+v", status)
	}
}
