package main

import (
	"context"
	"io"
	"log"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Runtime owns exactly one mapping's listener, cancellation and active connections.
type Runtime struct {
	mapping     Mapping
	proxy       Proxy
	listener    net.Listener
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.Mutex
	connections map[net.Conn]struct{}
	active      int
	lastError   string
	status      string
	stopped     bool
	slots       chan struct{}
	global      *atomic.Int64
	wait        sync.WaitGroup
}

// newRuntime binds a mapping or returns an explicit error state; disabled mappings never bind.
func newRuntime(mapping Mapping, proxy Proxy, slots chan struct{}, global *atomic.Int64) *Runtime {
	ctx, cancel := context.WithCancel(context.Background())
	runtime := &Runtime{mapping: mapping, proxy: proxy, ctx: ctx, cancel: cancel, connections: map[net.Conn]struct{}{}, status: "stopped", slots: slots, global: global}
	if !mapping.Enabled {
		return runtime
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort(mapping.ListenHost, strconv.Itoa(mapping.ListenPort)))
	if err != nil {
		runtime.status = "error"
		runtime.lastError = "listener unavailable; check address and port"
		log.Printf("mapping %s listener failed", mapping.ID)
		return runtime
	}
	runtime.listener = listener
	runtime.status = "running"
	runtime.wait.Add(1)
	go runtime.accept()
	log.Printf("mapping %s started", mapping.ID)
	return runtime
}

// accept admits ACL-approved connections without holding a mutex during network dialing.
func (runtime *Runtime) accept() {
	defer runtime.wait.Done()
	for {
		connection, err := runtime.listener.Accept()
		if err != nil {
			runtime.mu.Lock()
			if !runtime.stopped {
				runtime.status = "error"
				runtime.lastError = "listener accept failed"
				log.Printf("mapping %s accept failed", runtime.mapping.ID)
			}
			runtime.mu.Unlock()
			return
		}
		if tcp, ok := connection.(*net.TCPConn); ok {
			_ = tcp.SetKeepAlive(true)
			_ = tcp.SetKeepAlivePeriod(30 * time.Second)
		}
		if !runtime.allowed(connection.RemoteAddr()) {
			connection.Close()
			continue
		}
		runtime.mu.Lock()
		if runtime.stopped || runtime.active >= runtime.mapping.MaxConnections {
			runtime.mu.Unlock()
			connection.Close()
			continue
		}
		select {
		case runtime.slots <- struct{}{}:
			runtime.active++
			runtime.global.Add(1)
			runtime.connections[connection] = struct{}{}
			runtime.wait.Add(1)
			runtime.mu.Unlock()
			go runtime.forward(connection)
		default:
			runtime.mu.Unlock()
			connection.Close()
		}
	}
}

// allowed checks the remote IPv4 address against the configured CIDR allowlist.
func (runtime *Runtime) allowed(remote net.Addr) bool {
	host, _, err := net.SplitHostPort(remote.String())
	if err != nil {
		return false
	}
	address, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	for _, raw := range runtime.mapping.AllowedCIDRs {
		prefix, err := netip.ParsePrefix(raw)
		if err == nil && prefix.Contains(address.Unmap()) {
			return true
		}
	}
	return false
}

// forward establishes one bounded SOCKS5 tunnel and performs bidirectional copying.
// Half-close preserves responses after client EOF; a drain deadline bounds stuck peers.
func (runtime *Runtime) forward(client net.Conn) {
	defer runtime.wait.Done()
	defer func() {
		client.Close()
		runtime.mu.Lock()
		delete(runtime.connections, client)
		runtime.active--
		runtime.mu.Unlock()
		runtime.global.Add(-1)
		<-runtime.slots
	}()
	ctx, cancel := context.WithTimeout(runtime.ctx, 10*time.Second)
	upstream, err := dialSOCKS(ctx, runtime.proxy, runtime.mapping.TargetHost, runtime.mapping.TargetPort)
	cancel()
	if err != nil {
		runtime.mu.Lock()
		if !runtime.stopped {
			runtime.lastError = err.Error()
		}
		runtime.mu.Unlock()
		log.Printf("mapping %s upstream connection failed", runtime.mapping.ID)
		return
	}
	defer upstream.Close()
	runtime.mu.Lock()
	if runtime.stopped {
		runtime.mu.Unlock()
		return
	}
	runtime.connections[upstream] = struct{}{}
	runtime.lastError = ""
	runtime.mu.Unlock()
	defer func() { runtime.mu.Lock(); delete(runtime.connections, upstream); runtime.mu.Unlock() }()
	finished := make(chan struct{}, 2)
	copyStream := func(destination, source net.Conn) {
		_, copyErr := io.Copy(destination, source)
		if tcp, ok := destination.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		if copyErr != nil {
			client.Close()
			upstream.Close()
		}
		finished <- struct{}{}
	}
	go copyStream(upstream, client)
	go copyStream(client, upstream)
	<-finished
	_ = client.SetDeadline(time.Now().Add(30 * time.Second))
	_ = upstream.SetDeadline(time.Now().Add(30 * time.Second))
	<-finished
}

// Stop cancels pending dials and closes only this mapping's established streams, then waits for cleanup.
func (runtime *Runtime) Stop() {
	runtime.mu.Lock()
	if runtime.stopped {
		runtime.mu.Unlock()
		return
	}
	runtime.stopped = true
	runtime.status = "stopped"
	runtime.cancel()
	if runtime.listener != nil {
		runtime.listener.Close()
	}
	for connection := range runtime.connections {
		connection.Close()
	}
	runtime.mu.Unlock()
	runtime.wait.Wait()
	log.Printf("mapping %s stopped; active connections interrupted", runtime.mapping.ID)
}

// view returns a race-free API projection of configuration and transient status.
func (runtime *Runtime) view() MappingView {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return MappingView{Mapping: runtime.mapping, Status: runtime.status, ActiveConnections: runtime.active, LastError: runtime.lastError}
}

// MappingView augments configuration without exposing credentials.
type MappingView struct {
	Mapping
	Status            string `json:"status"`
	ActiveConnections int    `json:"activeConnections"`
	LastError         string `json:"lastError"`
}
