package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"time"
)

var (
	errRedisProtocol = errors.New("invalid Redis protocol")
	errRedisRejected = errors.New("Redis command rejected")
	errRedisOversize = errors.New("Redis response exceeds limit")
)

// probe uses one shared-slot, three-second connection for PING and INFO, returning only a sanitized snapshot.
func (monitor *redisMonitor) probe(ctx context.Context) redisStatus {
	result := monitor.base
	started := time.Now()
	checkedAt := started.UTC().Format(time.RFC3339)
	result.CheckedAt = &checkedAt
	connection, err := monitor.connect(ctx)
	if err != nil {
		return redisProbeFailure(result, err)
	}
	defer connection.Close()
	limited, reader := connection.limited, connection.reader
	if err = writeAll(connection, redisCommandBytes("PING")); err != nil {
		return redisProbeFailure(result, err)
	}
	pong, err := redisReply(reader, limited, false)
	if err != nil {
		return redisProbeFailure(result, err)
	}
	if string(pong) != "PONG" {
		return redisProbeFailure(result, errRedisProtocol)
	}
	if err = writeAll(connection, redisCommandBytes("INFO", "default")); err != nil {
		return redisProbeFailure(result, err)
	}
	info, err := redisReply(reader, limited, true)
	if err != nil {
		return redisProbeFailure(result, err)
	}
	metrics, databases, err := parseRedisInfo(string(info))
	if err != nil {
		return redisProbeFailure(result, err)
	}
	latency := float64(time.Since(started).Microseconds()) / 1000
	result.State, result.Error = "online", ""
	result.LatencyMs, result.Metrics, result.Databases = &latency, metrics, databases
	return result
}

// redisProbeFailure classifies transport failures separately from Redis errors and malformed responses.
func redisProbeFailure(base redisStatus, err error) redisStatus {
	switch {
	case errors.Is(err, errRedisRejected):
		return redisFailure(base, "error", "Redis 拒绝监控命令，请检查认证要求和命令权限")
	case errors.Is(err, errRedisOversize):
		return redisFailure(base, "error", "Redis 监控响应超过大小限制")
	case errors.Is(err, errRedisProtocol), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return redisFailure(base, "error", "Redis 监控响应格式无效")
	default:
		return redisFailure(base, "unreachable", "无法连接 Redis 或监控请求超时")
	}
}

// redisReply reuses the bounded RESP parser but keeps the status API's strict PONG/INFO-only reply contract.
// bulk chooses non-null bulk versus simple reply; the shared LimitedReader caps all command response bytes.
func redisReply(reader *bufio.Reader, limited *io.LimitedReader, bulk bool) ([]byte, error) {
	nodes := 1
	reply, err := readRedisRESP(reader, limited, 0, &nodes)
	if err != nil {
		return nil, err
	}
	expected := byte('+')
	if bulk {
		expected = '$'
	}
	if reply.kind != expected || reply.null {
		return nil, errRedisProtocol
	}
	return reply.text, nil
}
