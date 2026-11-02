package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// metricsTestConn reuses fakeHandshake while controlling response timing and CONNECT outcome.
// reject changes only the CONNECT reply; stall blocks the chosen phase until client cancellation.
type metricsTestConn struct {
	net.Conn
	reject bool
	stall  string
}

// Write delays greeting/CONNECT replies to make stage latency observable and returns transport errors.
// data is the existing fixture's response; stalled phases wait for the peer to close without replying.
func (connection metricsTestConn) Write(data []byte) (int, error) {
	phase := ""
	if len(data) == 2 && data[0] == 5 {
		phase = "proxy"
	} else if len(data) == 10 && data[0] == 5 {
		phase = "target"
	}
	if phase != "" {
		if connection.stall == phase {
			_, err := io.Copy(io.Discard, connection.Conn)
			if err == nil {
				err = io.EOF
			}
			return 0, err
		}
		timer := time.NewTimer(15 * time.Millisecond)
		<-timer.C
		if phase == "target" && connection.reject {
			data = append([]byte(nil), data...)
			data[1] = 5
		}
	}
	return connection.Conn.Write(data)
}

// TestConnectivityProxyStages verifies distinct authentication/target status and one TCP connection.
// Delayed fixture responses make successful latency values measurable without external services.
func TestConnectivityProxyStages(t *testing.T) {
	cases := []struct {
		name   string
		reject bool
		auth   bool
	}{
		{name: "success"},
		{name: "target rejection", reject: true},
		{name: "authentication failure", auth: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			var connections atomic.Int64
			proxy, _ := fakeSOCKSWithHandler(t, "test-user", "test-secret", func(connection net.Conn, user, password string, names chan<- string) {
				connections.Add(1)
				fakeHandshake(metricsTestConn{Conn: connection, reject: test.reject}, user, password, names)
			})
			if test.auth {
				proxy.Password = "wrong-test-secret"
			}
			runtime := &Runtime{proxy: proxy, mapping: Mapping{ID: "metrics", TargetHost: "db.internal", TargetPort: 5432}}
			point := probeConnectivity(context.Background(), runtime)
			if point.OK != (!test.reject && !test.auth) || point.Proxy.OK != !test.auth {
				t.Fatalf("incorrect stage status: %+v", point)
			}
			if point.Proxy.LatencyMs < 10 || point.LatencyMs < point.Proxy.LatencyMs || connections.Load() != 1 {
				t.Fatalf("incorrect latency or duplicate connection: %+v, connections=%d", point, connections.Load())
			}
			if !test.auth && point.LatencyMs-point.Proxy.LatencyMs < 10 {
				t.Fatalf("proxy latency incorrectly includes target phase: %+v", point)
			}
			if (point.Error != "") != !point.OK || (point.Proxy.Error != "") != !point.Proxy.OK {
				t.Fatalf("incorrect stage errors: %+v", point)
			}
			encoded, err := json.Marshal(point)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "test-secret") || strings.Contains(string(encoded), "test-user") {
				t.Fatal("credentials leaked into connectivity response")
			}
		})
	}
}

// TestSOCKSMetricsConcurrentCancellation checks simultaneous timeouts before and after authentication.
// Each timed-out dial returns promptly, keeps the correct phase result, and never returns a stream.
func TestSOCKSMetricsConcurrentCancellation(t *testing.T) {
	for _, phase := range []string{"proxy", "target"} {
		t.Run(phase, func(t *testing.T) {
			proxy, _ := fakeSOCKSWithHandler(t, "", "", func(connection net.Conn, user, password string, names chan<- string) {
				fakeHandshake(metricsTestConn{Conn: connection, stall: phase}, user, password, names)
			})
			var wait sync.WaitGroup
			for index := 0; index < 16; index++ {
				wait.Add(1)
				go func() {
					defer wait.Done()
					ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
					defer cancel()
					started := time.Now()
					connection, metrics, err := dialSOCKSWithMetrics(ctx, proxy, "db.internal", 5432)
					if connection != nil {
						connection.Close()
						t.Error("canceled dial returned a stream")
					}
					if err == nil || time.Since(started) > time.Second || metrics.OK != (phase == "target") {
						t.Errorf("incorrect canceled phase result: %+v, error=%v", metrics, err)
					}
					if (metrics.Error != "") != (phase == "proxy") {
						t.Errorf("incorrect canceled proxy error: %+v", metrics)
					}
				}()
			}
			wait.Wait()
		})
	}
}
