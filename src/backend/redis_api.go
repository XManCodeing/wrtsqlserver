package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// redisKey is display-only text paired with the lossless, canonical key identifier.
type redisKey struct {
	Key      string `json:"key"`
	KeyToken string `json:"keyToken"`
}

// redisKeysResult preserves every SCAN result, because COUNT is a hint, not a page-size guarantee.
type redisKeysResult struct {
	Database   string     `json:"database"`
	Keys       []redisKey `json:"keys"`
	NextCursor string     `json:"nextCursor"`
}

// canonicalUint validates unsigned decimal text without signs, whitespace or leading zeroes.
// bits controls the allowed range; a false result means input must not reach Redis.
func canonicalUint(text string, bits int) bool {
	value, err := strconv.ParseUint(text, 10, bits)
	return err == nil && strconv.FormatUint(value, 10) == text
}

// redisDisplayKey converts raw bytes to safe display text while preserving their base64url token.
// Invalid UTF-8 and controls use a clearly marked encoding rather than replacement characters.
func redisDisplayKey(raw []byte) redisKey {
	token := base64.RawURLEncoding.EncodeToString(raw)
	text := string(raw)
	safe := utf8.Valid(raw)
	for _, character := range text {
		if unicode.IsControl(character) {
			safe = false
			break
		}
	}
	if !safe {
		text = "[base64url:" + token + "]"
	}
	return redisKey{Key: text, KeyToken: token}
}

// redisQuery rejects unknown/duplicate parameters and invalid escaping; returns a bounded query map.
func redisQuery(request *http.Request, allowed ...string) (url.Values, error) {
	if len(request.URL.RawQuery) > 2*redisResponseMax {
		return nil, errRedisOversize
	}
	query, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil {
		return nil, errRedisProtocol
	}
	for name, values := range query {
		found := false
		for _, permitted := range allowed {
			if name == permitted {
				found = true
			}
		}
		if !found || len(values) != 1 {
			return nil, errRedisProtocol
		}
	}
	return query, nil
}

// handleRedisData dispatches authenticated routes without accepting client endpoints or command names.
// writer receives bounded JSON; request path and query validation happen before any network I/O.
func (app *App) handleRedisData(writer http.ResponseWriter, request *http.Request) {
	parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/api/redis/databases/"), "/")
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "db") || !canonicalUint(strings.TrimPrefix(parts[0], "db"), 16) {
		apiError(writer, 400, "数据库名称必须为 db0 至 db65535（不含前导零）")
		return
	}
	database, operation := parts[0], parts[1]
	if operation == "clear" && request.Method == "POST" {
		app.redis.clearDatabase(writer, request, database)
		return
	}
	if request.Method != "GET" || (operation != "keys" && operation != "value") {
		apiError(writer, 405, "不支持此 Redis 操作或请求方法")
		return
	}
	var result any
	var err error
	if operation == "keys" {
		result, err = app.redis.scanKeys(request, database)
	} else {
		result, err = app.redis.readValue(request, database)
	}
	if err != nil {
		redisAPIError(writer, err)
		return
	}
	writeRedisJSON(writer, result)
}

// writeRedisJSON enforces the final HTTP JSON size as well as the RESP wire cap, including escaping overhead.
// Oversized data is rejected in full, never shortened while retaining an advanced SCAN cursor.
func writeRedisJSON(writer http.ResponseWriter, result any) {
	encoded, err := json.Marshal(result)
	if err != nil {
		apiError(writer, 500, "Redis 响应编码失败")
		return
	}
	if len(encoded)+1 > redisResponseMax {
		redisAPIError(writer, errRedisOversize)
		return
	}
	writeJSON(writer, 200, json.RawMessage(encoded))
}

var errRedisInput = errors.New("invalid Redis request input")

// redisAPIError maps internal failures to safe public messages without exposing keys, values or server errors.
func redisAPIError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errRedisInput):
		apiError(writer, 400, "Redis 请求参数无效")
	case errors.Is(err, errRedisBusy):
		apiError(writer, 429, "Redis 请求繁忙，请稍后重试")
	case errors.Is(err, errRedisOversize):
		apiError(writer, 502, "Redis 响应超过 1 MiB 或元素数量限制，未返回不完整数据")
	case errors.Is(err, errRedisRejected):
		apiError(writer, 502, "Redis 拒绝命令，请检查数据库范围、认证、权限或键类型变化")
	case errors.Is(err, errRedisProtocol):
		apiError(writer, 502, "Redis 响应格式无效")
	default:
		apiError(writer, 504, "Redis 请求超时、取消或连接失败")
	}
}

// scanKeys validates cursor/pattern/count, SELECTs the database, then returns all bounded SCAN results.
// COUNT remains fixed at 50; no sorting/truncation changes cursor semantics or drops returned keys.
func (monitor *redisMonitor) scanKeys(request *http.Request, database string) (redisKeysResult, error) {
	result := redisKeysResult{Database: database, Keys: []redisKey{}}
	query, err := redisQuery(request, "cursor", "pattern", "count")
	if err != nil {
		return result, errRedisInput
	}
	cursor, pattern := "0", "*"
	if query.Has("cursor") {
		cursor = query.Get("cursor")
	}
	if query.Has("pattern") {
		pattern = query.Get("pattern")
	}
	if !canonicalUint(cursor, 64) || len(pattern) > 256 || (query.Has("count") && query.Get("count") != "50") {
		return result, errRedisInput
	}
	connection, err := monitor.connect(request.Context())
	if err != nil {
		return result, err
	}
	defer connection.Close()
	if err = connection.selectDatabase(database); err != nil {
		return result, err
	}
	reply, err := connection.command("SCAN", cursor, "MATCH", pattern, "COUNT", "50")
	if err != nil {
		return result, err
	}
	next, items, err := redisScanParts(reply)
	if err != nil {
		return result, err
	}
	result.NextCursor = next
	for _, item := range items {
		result.Keys = append(result.Keys, redisDisplayKey(item.text))
	}
	return result, nil
}

// redisScanParts validates a SCAN/HSCAN/SSCAN pair and every bulk element, returning the full array.
func redisScanParts(reply redisRESP) (string, []redisRESP, error) {
	if reply.kind != '*' || reply.null || len(reply.items) != 2 {
		return "", nil, errRedisProtocol
	}
	cursor, items := reply.items[0], reply.items[1]
	if cursor.kind != '$' || cursor.null || !canonicalUint(string(cursor.text), 64) {
		return "", nil, errRedisProtocol
	}
	if err := redisBulkArray(items); err != nil {
		return "", nil, err
	}
	return string(cursor.text), items.items, nil
}

// redisBulkArray requires an ordinary array containing only non-null bulk strings; returns validation error.
func redisBulkArray(reply redisRESP) error {
	if reply.kind != '*' || reply.null {
		return errRedisProtocol
	}
	for _, item := range reply.items {
		if item.kind != '$' || item.null {
			return errRedisProtocol
		}
	}
	return nil
}

// beginClear atomically takes a nonblocking mutation lock and a five-second global cooldown.
// Invalid input never consumes the cooldown; accepted attempts do even if their transport fails.
func (monitor *redisMonitor) beginClear() bool {
	monitor.mu.Lock()
	defer monitor.mu.Unlock()
	if monitor.clearing || time.Since(monitor.lastClear) < 5*time.Second {
		return false
	}
	monitor.clearing, monitor.lastClear = true, time.Now()
	monitor.generation++
	monitor.expires = time.Time{}
	return true
}

// endClear invalidates both cached and in-flight pre-completion snapshots, including uncertain outcomes.
func (monitor *redisMonitor) endClear() {
	monitor.mu.Lock()
	defer monitor.mu.Unlock()
	monitor.clearing = false
	monitor.generation++
	monitor.expires = time.Time{}
}

// clearDatabase validates exact confirmation/address, then issues SELECT and only FLUSHDB ASYNC once.
// accepted means Redis acknowledged the operation, not that background reclamation or future writes ended.
func (monitor *redisMonitor) clearDatabase(writer http.ResponseWriter, request *http.Request, database string) {
	var body struct {
		Confirmation string `json:"confirmation"`
		Address      string `json:"address"`
	}
	if _, err := redisQuery(request); err != nil {
		redisAPIError(writer, errRedisInput)
		return
	}
	if !decodeJSON(writer, request, &body) {
		return
	}
	if body.Confirmation != "清空 "+database {
		apiError(writer, 400, "请输入精确的数据库清空确认文字")
		return
	}
	if body.Address == "" || body.Address != monitor.base.Address {
		apiError(writer, 409, "Redis 地址已变化，请刷新状态后重新确认")
		return
	}
	if !monitor.beginClear() {
		writer.Header().Set("Retry-After", "5")
		redisAPIError(writer, errRedisBusy)
		return
	}
	defer monitor.endClear()
	success := false
	defer func() { log.Printf("Redis clear database=%s success=%t", database, success) }()
	connection, err := monitor.connect(request.Context())
	if err != nil {
		redisAPIError(writer, err)
		return
	}
	defer connection.Close()
	if err = connection.selectDatabase(database); err != nil {
		redisAPIError(writer, err)
		return
	}
	reply, err := connection.command("FLUSHDB", "ASYNC")
	if errors.Is(err, errRedisRejected) {
		redisAPIError(writer, err)
		return
	}
	if err != nil || reply.kind != '+' || string(reply.text) != "OK" {
		apiError(writer, 504, "清空命令已尝试发送，执行结果不确定；请刷新检查，禁止自动重试")
		return
	}
	success = true
	writeRedisJSON(writer, struct {
		Database string `json:"database"`
		Accepted bool   `json:"accepted"`
	}{database, true})
}
