package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
)

const redisElementMax = 8192

var errRedisBusy = errors.New("Redis request limit reached")

// redisRESP represents a bounded RESP2 reply; kind preserves simple/bulk/null distinctions.
type redisRESP struct {
	kind    byte
	text    []byte
	integer int64
	items   []redisRESP
	null    bool
}

// readRedisRESP parses reader using a shared byte/node budget and at most three array levels.
// limited covers all replies on one connection; errors never contain server payloads.
func readRedisRESP(reader *bufio.Reader, limited *io.LimitedReader, depth int, nodes *int) (redisRESP, error) {
	var result redisRESP
	*nodes--
	if *nodes < 0 || depth > 3 {
		return result, errRedisOversize
	}
	line, err := reader.ReadSlice('\n')
	if err != nil {
		if errors.Is(err, bufio.ErrBufferFull) || limited.N == 0 {
			return result, errRedisOversize
		}
		return result, err
	}
	if len(line) < 3 || line[len(line)-2] != '\r' {
		return result, errRedisProtocol
	}
	result.kind = line[0]
	text := string(line[1 : len(line)-2])
	switch result.kind {
	case '-':
		return result, errRedisRejected
	case '+':
		result.text = []byte(text)
	case ':':
		result.integer, err = strconv.ParseInt(text, 10, 64)
		if err != nil || strconv.FormatInt(result.integer, 10) != text {
			return result, errRedisProtocol
		}
	case '$', '*':
		length, parseErr := strconv.ParseInt(text, 10, 64)
		if parseErr != nil || strconv.FormatInt(length, 10) != text || length < -1 {
			return result, errRedisProtocol
		}
		if length == -1 {
			result.null = true
			return result, nil
		}
		if result.kind == '$' {
			if length > redisResponseMax-2 || length+2 > limited.N+int64(reader.Buffered()) {
				return result, errRedisOversize
			}
			payload := make([]byte, int(length)+2)
			if _, err = io.ReadFull(reader, payload); err != nil {
				return result, err
			}
			if payload[len(payload)-2] != '\r' || payload[len(payload)-1] != '\n' {
				return result, errRedisProtocol
			}
			result.text = payload[:len(payload)-2]
		} else {
			if length > int64(*nodes) || length > redisElementMax {
				return result, errRedisOversize
			}
			result.items = make([]redisRESP, int(length))
			for index := range result.items {
				result.items[index], err = readRedisRESP(reader, limited, depth+1, nodes)
				if err != nil {
					return result, err
				}
			}
		}
	default:
		return result, errRedisProtocol
	}
	return result, nil
}

// redisCommandBytes encodes explicit command arguments as binary-safe RESP without interpolation.
// Callers supply only hard-coded command names and validated arguments; returns wire bytes.
func redisCommandBytes(arguments ...string) []byte {
	var output bytes.Buffer
	fmt.Fprintf(&output, "*%d\r\n", len(arguments))
	for _, argument := range arguments {
		fmt.Fprintf(&output, "$%d\r\n%s\r\n", len(argument), argument)
	}
	return output.Bytes()
}

// redisConnection owns one request's fixed-endpoint connection, timeout, and aggregate reply budget.
type redisConnection struct {
	net.Conn
	reader  *bufio.Reader
	limited *io.LimitedReader
	cleanup func()
	nodes   int
}

// acquireSlot reserves one of four shared network operations without queueing; returns busy on overload.
func (monitor *redisMonitor) acquireSlot() error {
	select {
	case monitor.slots <- struct{}{}:
		return nil
	default:
		return errRedisBusy
	}
}

// connect selects the configured endpoint only and binds its lifetime to request and monitor cancellation.
// The caller owns Close; failure releases the shared slot and never retries.
func (monitor *redisMonitor) connect(parent context.Context) (*redisConnection, error) {
	if err := monitor.acquireSlot(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(parent, redisProbeTimeout)
	stopMonitor := context.AfterFunc(monitor.ctx, cancel)
	release := func() { stopMonitor(); cancel(); <-monitor.slots }
	if monitor.ctx.Err() != nil {
		release()
		return nil, context.Canceled
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", monitor.base.Address)
	if err != nil {
		release()
		return nil, err
	}
	stopClose := context.AfterFunc(ctx, func() { _ = connection.Close() })
	deadline, _ := ctx.Deadline()
	if err = connection.SetDeadline(deadline); err != nil {
		stopClose()
		_ = connection.Close()
		release()
		return nil, err
	}
	limited := &io.LimitedReader{R: connection, N: redisResponseMax}
	return &redisConnection{Conn: connection, reader: bufio.NewReaderSize(limited, 4096), limited: limited,
		nodes: redisElementMax, cleanup: func() { stopClose(); release() }}, nil
}

// Close releases the connection and slot once after the owning request completes; returns transport error.
func (connection *redisConnection) Close() error {
	err := connection.Conn.Close()
	connection.cleanup()
	return err
}

// command writes a fixed operation once and parses its reply against the connection-wide budgets.
func (connection *redisConnection) command(arguments ...string) (redisRESP, error) {
	if err := writeAll(connection.Conn, redisCommandBytes(arguments...)); err != nil {
		return redisRESP{}, err
	}
	return readRedisRESP(connection.reader, connection.limited, 0, &connection.nodes)
}

// selectDatabase requires an explicit simple +OK, preventing any later operation on a wrong database.
func (connection *redisConnection) selectDatabase(database string) error {
	reply, err := connection.command("SELECT", database[2:])
	if err != nil {
		return err
	}
	if reply.kind != '+' || string(reply.text) != "OK" {
		return errRedisProtocol
	}
	return nil
}
