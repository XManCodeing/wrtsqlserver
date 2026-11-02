package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

// dialSOCKS connects through endpoint with remote DNS and optional RFC1929 credentials.
// ctx bounds both TCP dialing and the complete handshake; returned streams have no deadline.
func dialSOCKS(ctx context.Context, endpoint Proxy, host string, port int) (net.Conn, error) {
	connection, _, err := dialSOCKSWithMetrics(ctx, endpoint, host, port)
	return connection, err
}

// dialSOCKSWithMetrics returns the same tunnel plus endpoint dialing/authentication metrics.
// ctx, endpoint and host/port retain dialSOCKS semantics; metrics stop before target CONNECT.
func dialSOCKSWithMetrics(ctx context.Context, endpoint Proxy, host string, port int) (net.Conn, ProxyConnectivity, error) {
	started := time.Now()
	metrics := ProxyConnectivity{}
	connection, err := dialSOCKSObserved(ctx, endpoint, host, port, func() {
		metrics.OK = true
		metrics.LatencyMs = time.Since(started).Milliseconds()
	})
	if !metrics.OK {
		metrics.LatencyMs = time.Since(started).Milliseconds()
		if err != nil {
			metrics.Error = err.Error()
		}
	}
	return connection, metrics, err
}

// dialSOCKSObserved uses one connection for endpoint authentication and target CONNECT.
// authenticated is called synchronously only after successful authentication; returns tunnel/error.
func dialSOCKSObserved(ctx context.Context, endpoint Proxy, host string, port int, authenticated func()) (net.Conn, error) {
	dialer := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	connection, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(endpoint.Host, strconv.Itoa(endpoint.Port)))
	if err != nil {
		return nil, errors.New("SOCKS5 endpoint connection failed")
	}
	complete := false
	defer func() {
		if !complete {
			connection.Close()
		}
	}()
	stopped := make(chan struct{})
	stopCancel := context.AfterFunc(ctx, func() { connection.Close(); close(stopped) })
	defer func() {
		if !stopCancel() {
			<-stopped
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := connection.SetDeadline(deadline); err != nil {
		return nil, err
	}
	if err := socksAuthenticate(connection, endpoint); err != nil {
		return nil, err
	}
	authenticated()
	request := []byte{5, 1, 0}
	if address := net.ParseIP(host); address != nil {
		if ipv4 := address.To4(); ipv4 != nil {
			request = append(request, 1)
			request = append(request, ipv4...)
		} else {
			request = append(request, 4)
			request = append(request, address.To16()...)
		}
	} else {
		request = append(request, 3, byte(len(host)))
		request = append(request, host...)
	}
	request = binary.BigEndian.AppendUint16(request, uint16(port))
	if err := writeAll(connection, request); err != nil {
		return nil, errors.New("SOCKS5 request write failed")
	}
	reply := make([]byte, 4)
	if _, err := io.ReadFull(connection, reply); err != nil {
		return nil, errors.New("SOCKS5 reply failed")
	}
	if reply[0] != 5 || reply[1] != 0 || reply[2] != 0 {
		return nil, fmt.Errorf("SOCKS5 connect rejected (code %d)", reply[1])
	}
	size := 0
	switch reply[3] {
	case 1:
		size = 4
	case 4:
		size = 16
	case 3:
		length := []byte{0}
		if _, err := io.ReadFull(connection, length); err != nil {
			return nil, errors.New("SOCKS5 reply truncated")
		}
		size = int(length[0])
	default:
		return nil, errors.New("SOCKS5 invalid reply address")
	}
	if _, err := io.CopyN(io.Discard, connection, int64(size+2)); err != nil {
		return nil, errors.New("SOCKS5 reply truncated")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := connection.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	complete = true
	return connection, nil
}

// socksAuthenticate negotiates a strict authentication method and verifies server acceptance.
func socksAuthenticate(connection net.Conn, endpoint Proxy) error {
	method := byte(0)
	if endpoint.Username != "" || endpoint.Password != "" {
		method = 2
	}
	if err := writeAll(connection, []byte{5, 1, method}); err != nil {
		return errors.New("SOCKS5 greeting write failed")
	}
	reply := []byte{0, 0}
	if _, err := io.ReadFull(connection, reply); err != nil {
		return errors.New("SOCKS5 greeting read failed")
	}
	if reply[0] != 5 || reply[1] != method {
		return errors.New("SOCKS5 authentication method rejected")
	}
	if method == 0 {
		return nil
	}
	credentials := []byte{1, byte(len(endpoint.Username))}
	credentials = append(credentials, endpoint.Username...)
	credentials = append(credentials, byte(len(endpoint.Password)))
	credentials = append(credentials, endpoint.Password...)
	if err := writeAll(connection, credentials); err != nil {
		return errors.New("SOCKS5 authentication write failed")
	}
	if _, err := io.ReadFull(connection, reply); err != nil {
		return errors.New("SOCKS5 authentication read failed")
	}
	if reply[0] != 1 || reply[1] != 0 {
		return errors.New("SOCKS5 authentication rejected")
	}
	return nil
}

// writeAll writes all bytes or returns a short-write/transport error.
func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		count, err := writer.Write(data)
		if err != nil {
			return err
		}
		if count == 0 {
			return io.ErrShortWrite
		}
		data = data[count:]
	}
	return nil
}
