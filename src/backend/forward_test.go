package main

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSOCKS accepts real SOCKS5 handshakes and echoes payloads without sending database commands.
// Authentication is mandatory when user is nonempty; target names are reported to names.
func fakeSOCKS(t *testing.T, user, password string) (Proxy, <-chan string) {
	t.Helper()
	return fakeSOCKSWithHandler(t, user, password, fakeHandshake)
}

// fakeSOCKSWithHandler returns a local proxy and targets using handler for each accepted stream.
// user/password configure test credentials; cleanup cancels all connections and joins handlers.
func fakeSOCKSWithHandler(t *testing.T, user, password string, handler func(net.Conn, string, string, chan<- string)) (Proxy, <-chan string) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	names := make(chan string, 32)
	var wait sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	wait.Add(1)
	go func() {
		defer wait.Done()
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			wait.Add(1)
			go func() {
				defer wait.Done()
				defer connection.Close()
				stop := context.AfterFunc(ctx, func() { connection.Close() })
				defer stop()
				handler(connection, user, password, names)
			}()
		}
	}()
	t.Cleanup(func() { cancel(); listener.Close(); wait.Wait() })
	return Proxy{ID: "proxy", Name: "fake", Host: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port, Username: user, Password: password}, names
}

// fakeHandshake processes a bounded greeting/auth/connect exchange and echoes the connected stream.
func fakeHandshake(connection net.Conn, user, password string, names chan<- string) {
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	greeting := make([]byte, 3)
	if _, err := io.ReadFull(connection, greeting); err != nil {
		return
	}
	method := byte(0)
	if user != "" {
		method = 2
	}
	if greeting[0] != 5 || greeting[1] != 1 || greeting[2] != method {
		return
	}
	if writeAll(connection, []byte{5, method}) != nil {
		return
	}
	if method == 2 {
		header := make([]byte, 2)
		if _, err := io.ReadFull(connection, header); err != nil {
			return
		}
		username := make([]byte, int(header[1]))
		if _, err := io.ReadFull(connection, username); err != nil {
			return
		}
		length := []byte{0}
		if _, err := io.ReadFull(connection, length); err != nil {
			return
		}
		secret := make([]byte, int(length[0]))
		if _, err := io.ReadFull(connection, secret); err != nil {
			return
		}
		if header[0] != 1 || string(username) != user || string(secret) != password {
			_ = writeAll(connection, []byte{1, 1})
			return
		}
		if writeAll(connection, []byte{1, 0}) != nil {
			return
		}
	}
	header := make([]byte, 4)
	if _, err := io.ReadFull(connection, header); err != nil {
		return
	}
	size := 0
	switch header[3] {
	case 1:
		size = 4
	case 4:
		size = 16
	case 3:
		length := []byte{0}
		if _, err := io.ReadFull(connection, length); err != nil {
			return
		}
		size = int(length[0])
	default:
		return
	}
	address := make([]byte, size)
	if _, err := io.ReadFull(connection, address); err != nil {
		return
	}
	port := make([]byte, 2)
	if _, err := io.ReadFull(connection, port); err != nil {
		return
	}
	if header[3] == 3 {
		names <- string(address) + ":" + strconv.Itoa(int(binary.BigEndian.Uint16(port)))
	}
	if writeAll(connection, []byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0}) != nil {
		return
	}
	_ = connection.SetDeadline(time.Time{})
	_, _ = io.Copy(connection, connection)
}

// runtimeForTest creates a real ephemeral listener while bypassing production's nonzero port validation.
func runtimeForTest(t *testing.T, proxy Proxy, acl []string, limit int, slots chan struct{}, active *atomic.Int64) *Runtime {
	t.Helper()
	mapping := Mapping{ID: "mapping", Name: "DB", ListenHost: "127.0.0.1", ListenPort: 0, TargetHost: "unresolved.internal", TargetPort: 5432, ProxyID: proxy.ID, Enabled: true, MaxConnections: limit, AllowedCIDRs: acl}
	runtime := newRuntime(mapping, proxy, slots, active)
	if runtime.view().Status != "running" {
		t.Fatal(runtime.view())
	}
	t.Cleanup(runtime.Stop)
	return runtime
}

// connectRuntime connects a client with a bounded deadline to the runtime listener.
func connectRuntime(t *testing.T, runtime *Runtime) net.Conn {
	t.Helper()
	connection, err := net.DialTimeout("tcp", runtime.listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close() })
	return connection
}

// assertEcho verifies a complete bidirectional byte stream, without interpreting or logging payloads.
func assertEcho(t *testing.T, connection net.Conn) {
	t.Helper()
	payload := []byte("opaque database transport bytes")
	if err := writeAll(connection, payload); err != nil {
		t.Fatal(err)
	}
	result := make([]byte, len(payload))
	if _, err := io.ReadFull(connection, result); err != nil {
		t.Fatal(err)
	}
	if string(result) != string(payload) {
		t.Fatal("echo mismatch")
	}
}

// TestSOCKSForwardRemoteDNSAndStop verifies auth, remote DNS, duplex forwarding and deterministic shutdown cleanup.
func TestSOCKSForwardRemoteDNSAndStop(t *testing.T) {
	proxy, names := fakeSOCKS(t, "user", "secret")
	var active atomic.Int64
	slots := make(chan struct{}, 4)
	runtime := runtimeForTest(t, proxy, []string{"127.0.0.1/32"}, 2, slots, &active)
	connection := connectRuntime(t, runtime)
	assertEcho(t, connection)
	select {
	case name := <-names:
		if name != "unresolved.internal:5432" {
			t.Fatal(name)
		}
	case <-time.After(time.Second):
		t.Fatal("remote DNS not observed")
	}
	if runtime.view().ActiveConnections != 1 || active.Load() != 1 {
		t.Fatal("active count incorrect")
	}
	runtime.Stop()
	if runtime.view().ActiveConnections != 0 || active.Load() != 0 || len(slots) != 0 {
		t.Fatal("connection resources leaked")
	}
	buffer := make([]byte, 1)
	if _, err := connection.Read(buffer); err == nil {
		t.Fatal("connection not closed")
	}
}

// TestACLAndConnectionLimits rejects unauthorized peers and enforces mapping/global limits.
func TestACLAndConnectionLimits(t *testing.T) {
	proxy, _ := fakeSOCKS(t, "", "")
	var active atomic.Int64
	slots := make(chan struct{}, 1)
	denied := runtimeForTest(t, proxy, []string{"10.0.0.0/8"}, 2, slots, &active)
	connection := connectRuntime(t, denied)
	buffer := make([]byte, 1)
	if _, err := connection.Read(buffer); err == nil {
		t.Fatal("ACL accepted localhost")
	}
	if active.Load() != 0 {
		t.Fatal("denied connection consumed resources")
	}
	allowed := runtimeForTest(t, proxy, []string{"127.0.0.0/8"}, 1, slots, &active)
	first := connectRuntime(t, allowed)
	assertEcho(t, first)
	second := connectRuntime(t, allowed)
	if _, err := second.Read(buffer); err == nil {
		t.Fatal("mapping limit not enforced")
	}
	other := runtimeForTest(t, proxy, []string{"127.0.0.0/8"}, 1, slots, &active)
	third := connectRuntime(t, other)
	if _, err := third.Read(buffer); err == nil {
		t.Fatal("global limit not enforced")
	}
	other.Stop()
	assertEcho(t, first)
	allowed.Stop()
	if active.Load() != 0 {
		t.Fatal("active counter leaked")
	}
}

// TestSOCKSCancellation closes a silent server during greeting and verifies timeout cancellation.
func TestSOCKSCancellation(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		connection, err := listener.Accept()
		if err == nil {
			accepted <- connection
		}
	}()
	proxy := Proxy{Host: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	connection, err := dialSOCKS(ctx, proxy, "db.internal", 5432)
	if connection != nil {
		connection.Close()
	}
	if err == nil || time.Since(started) > time.Second {
		t.Fatal("dial cancellation failed", err)
	}
	select {
	case connection := <-accepted:
		connection.Close()
	case <-time.After(time.Second):
		t.Fatal("server did not accept")
	}
}

// TestSOCKSAuthenticationFailure verifies incorrect credentials cannot open an upstream tunnel.
func TestSOCKSAuthenticationFailure(t *testing.T) {
	proxy, _ := fakeSOCKS(t, "user", "secret")
	proxy.Password = "wrong"
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	connection, err := dialSOCKS(ctx, proxy, "db.internal", 5432)
	if connection != nil {
		connection.Close()
	}
	if err == nil {
		t.Fatal("wrong credentials accepted")
	}
}
