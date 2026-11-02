package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// redisTestStep pairs exact command arguments with a canned server reply; empty reply waits for client close.
type redisTestStep struct {
	arguments []string
	reply     string
}

// redisSteps serves only the prescribed command sequence and rejects every additional command.
// It returns a loopback endpoint and thus cannot reach any production Redis instance.
func redisSteps(t *testing.T, steps ...redisTestStep) string {
	t.Helper()
	address, _ := fakeRedis(t, func(connection net.Conn) {
		reader := bufio.NewReader(connection)
		for _, step := range steps {
			if !expectRedisCommand(t, reader, string(redisCommandBytes(step.arguments...))) {
				return
			}
			if step.reply == "" {
				break
			}
			if err := writeAll(connection, []byte(step.reply)); err != nil {
				t.Error(err)
				return
			}
		}
		if extra, err := reader.ReadByte(); err == nil {
			t.Errorf("unexpected command byte: %q", extra)
		}
	})
	return address
}

// redisArray builds a bulk-only RESP array fixture, preserving binary bytes and empty strings.
func redisArray(values ...string) string {
	result := fmt.Sprintf("*%d\r\n", len(values))
	for _, value := range values {
		result += redisBulk(value)
	}
	return result
}

// TestRedisDataAuthAndValidation verifies session, CSRF, same-origin, methods and all input boundaries before dialing.
func TestRedisDataAuthAndValidation(t *testing.T) {
	address, connections := fakeRedis(t, func(connection net.Conn) { t.Error("invalid request connected") })
	t.Setenv("REDIS_ADDR", address)
	app := testApp(t)
	cookie, csrf := loginTest(t, app)
	for _, operation := range []string{"keys", "value", "clear"} {
		method := "GET"
		if operation == "clear" {
			method = "POST"
		}
		if response := requestAPI(app, method, "/api/redis/databases/db0/"+operation, "{}", nil, ""); response.Code != 401 {
			t.Fatal(response.Code)
		}
	}
	invalid := []string{
		"db00/keys", "db65536/keys", "db-1/keys", "db+1/keys", "db/keys",
		"db0/keys?cursor=01", "db0/keys?cursor=-1", "db0/keys?cursor=18446744073709551616",
		"db0/keys?cursor=0&cursor=1", "db0/keys?count=51", "db0/keys?pattern=" + strings.Repeat("x", 257),
		"db0/keys?address=evil:1", "db0/value", "db0/value?keyToken=YQ==", "db0/value?keyToken=_x", "db0/value?keyToken=%25",
	}
	for _, path := range invalid {
		response := requestAPI(app, "GET", "/api/redis/databases/"+path, "", cookie, "")
		if response.Code != 400 {
			t.Errorf("%s: %d", path, response.Code)
		}
	}
	body := fmt.Sprintf(`{"confirmation":"清空 db0","address":%q}`, address)
	if response := requestAPI(app, "POST", "/api/redis/databases/db0/clear", body, cookie, ""); response.Code != 403 {
		t.Fatal(response.Code)
	}
	request := httptest.NewRequest("POST", "http://localhost/api/redis/databases/db0/clear", strings.NewReader(body))
	request.AddCookie(cookie)
	request.Header.Set("X-CSRF-Token", csrf)
	request.Header.Set("Origin", "https://evil.example")
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != 403 {
		t.Fatal(response.Code)
	}
	for _, body := range []string{`{"confirmation":"清空 db1","address":"x"}`, `{"confirmation":"清空 db0","address":"old:6379"}`} {
		if response := requestAPI(app, "POST", "/api/redis/databases/db0/clear", body, cookie, csrf); response.Code != 400 && response.Code != 409 {
			t.Fatal(response.Code)
		}
	}
	if response := requestAPI(app, "GET", "/api/redis/databases/db0/clear", "", cookie, ""); response.Code != 405 {
		t.Fatal(response.Code)
	}
	if connections.Load() != 0 {
		t.Fatal("invalid input reached Redis")
	}
}

// TestRedisScanCountHintAndBinary ensures an oversized SCAN page is not truncated or addressed by display text.
func TestRedisScanCountHintAndBinary(t *testing.T) {
	keys := []string{"", "\xff\x00\r\n", "中文"}
	for index := 0; index < 70; index++ {
		keys = append(keys, fmt.Sprintf("key%d", index))
	}
	address := redisSteps(t,
		redisTestStep{[]string{"SELECT", "65535"}, "+OK\r\n"},
		redisTestStep{[]string{"SCAN", "18446744073709551615", "MATCH", "*", "COUNT", "50"}, "*2\r\n" + redisBulk("123") + redisArray(keys...)})
	t.Setenv("REDIS_ADDR", address)
	app := testApp(t)
	cookie, _ := loginTest(t, app)
	response := requestAPI(app, "GET", "/api/redis/databases/db65535/keys?cursor=18446744073709551615&count=50", "", cookie, "")
	var result redisKeysResult
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil {
		t.Fatal(response.Code, response.Body.String())
	}
	if len(result.Keys) != len(keys) || result.NextCursor != "123" {
		t.Fatalf("dropped SCAN keys: %+v", result)
	}
	for index, key := range keys {
		decoded, err := base64.RawURLEncoding.DecodeString(result.Keys[index].KeyToken)
		if err != nil || string(decoded) != key {
			t.Fatal("lossy token")
		}
	}
	if !strings.HasPrefix(result.Keys[1].Key, "[base64url:") {
		t.Fatal("binary key not marked")
	}
}

// TestRedisValuePreviews covers all supported types, real truncation evidence, binary values and absent/unsupported keys.
func TestRedisValuePreviews(t *testing.T) {
	cases := []struct {
		name, kind, ttl string
		steps           []redisTestStep
		encoding        string
		truncated       bool
	}{
		{"string", "string", ":-1\r\n", []redisTestStep{{[]string{"GETRANGE", "\xff", "0", "4096"}, redisBulk(strings.Repeat("x", 4097))}}, "utf8", true},
		{"binary", "string", ":20\r\n", []redisTestStep{{[]string{"GETRANGE", "\xff", "0", "4096"}, redisBulk("\xff\x00")}}, "base64", false},
		{"list", "list", ":-1\r\n", []redisTestStep{{[]string{"LRANGE", "\xff", "0", "19"}, redisArray("second", "first")}, {[]string{"LLEN", "\xff"}, ":30\r\n"}}, "json", true},
		{"set", "set", ":-1\r\n", []redisTestStep{{[]string{"SSCAN", "\xff", "0", "COUNT", "20"}, "*2\r\n" + redisBulk("0") + redisArray("z", "a", "\xff")}}, "json", false},
		{"hash", "hash", ":-1\r\n", []redisTestStep{{[]string{"HSCAN", "\xff", "0", "COUNT", "20"}, "*2\r\n" + redisBulk("9") + redisArray("z", "last", "a", "\xff")}}, "json", true},
		{"zset", "zset", ":-1\r\n", []redisTestStep{{[]string{"ZRANGE", "\xff", "0", "19", "WITHSCORES"}, redisArray("member", "1.5")}, {[]string{"ZCARD", "\xff"}, ":1\r\n"}}, "json", false},
		{"missing", "none", ":-2\r\n", nil, "utf8", false},
		{"expired", "string", ":-2\r\n", nil, "utf8", false},
		{"stream", "stream", ":-1\r\n", nil, "utf8", false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			steps := []redisTestStep{{[]string{"SELECT", "2"}, "+OK\r\n"}, {[]string{"TYPE", "\xff"}, "+" + testCase.kind + "\r\n"}, {[]string{"TTL", "\xff"}, testCase.ttl}}
			address := redisSteps(t, append(steps, testCase.steps...)...)
			t.Setenv("REDIS_ADDR", address)
			app := testApp(t)
			cookie, _ := loginTest(t, app)
			response := requestAPI(app, "GET", "/api/redis/databases/db2/value?keyToken=_w", "", cookie, "")
			var result redisValueResult
			if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil {
				t.Fatal(response.Code, response.Body.String())
			}
			if result.Encoding != testCase.encoding || result.Truncated != testCase.truncated || result.KeyToken != "_w" || result.Value == "" {
				t.Fatalf("bad preview: %+v", result)
			}
			if testCase.name == "expired" && result.Type != "none" {
				t.Fatal("expired key not reported")
			}
			if testCase.name == "hash" && !strings.HasPrefix(result.Value, `[[{"value":"a"`) {
				t.Fatal("hash not sorted")
			}
		})
	}
}

// TestRedisClearSelectFailureNeverFlush requires exact +OK before mutation and prevents every fallback/retry.
func TestRedisClearSelectFailureNeverFlush(t *testing.T) {
	for _, reply := range []string{"-ERR DB index is out of range\r\n", "+NO\r\n", redisBulk("OK"), ":1\r\n"} {
		t.Run(url.QueryEscape(reply), func(t *testing.T) {
			address := redisSteps(t, redisTestStep{[]string{"SELECT", "9"}, reply})
			t.Setenv("REDIS_ADDR", address)
			app := testApp(t)
			cookie, csrf := loginTest(t, app)
			body := fmt.Sprintf(`{"confirmation":"清空 db9","address":%q}`, address)
			response := requestAPI(app, "POST", "/api/redis/databases/db9/clear", body, cookie, csrf)
			if response.Code != 502 {
				t.Fatal(response.Code, response.Body.String())
			}
		})
	}
}

// TestRedisClearSelectedOnly checks command scope, asynchronous acceptance and global cooldown.
func TestRedisClearSelectedOnly(t *testing.T) {
	address := redisSteps(t, redisTestStep{[]string{"SELECT", "3"}, "+OK\r\n"}, redisTestStep{[]string{"FLUSHDB", "ASYNC"}, "+OK\r\n"})
	t.Setenv("REDIS_ADDR", address)
	app := testApp(t)
	cookie, csrf := loginTest(t, app)
	app.redis.mu.Lock()
	app.redis.cached = app.redis.base
	app.redis.expires = time.Now().Add(time.Minute)
	app.redis.mu.Unlock()
	body := fmt.Sprintf(`{"confirmation":"清空 db3","address":%q}`, address)
	response := requestAPI(app, "POST", "/api/redis/databases/db3/clear", body, cookie, csrf)
	if response.Code != 200 || strings.TrimSpace(response.Body.String()) != `{"database":"db3","accepted":true}` {
		t.Fatal(response.Code, response.Body.String())
	}
	app.redis.mu.Lock()
	expired := app.redis.expires.IsZero()
	app.redis.mu.Unlock()
	if !expired {
		t.Fatal("cache not invalidated")
	}
	response = requestAPI(app, "POST", "/api/redis/databases/db3/clear", body, cookie, csrf)
	if response.Code != 429 {
		t.Fatal("cooldown missing", response.Code)
	}
}

// TestRedisDataTimeoutAndUncertainClear checks one three-second deadline and explicit no-retry mutation errors.
func TestRedisDataTimeoutAndUncertainClear(t *testing.T) {
	for _, clear := range []bool{false, true} {
		t.Run(fmt.Sprint(clear), func(t *testing.T) {
			arguments := []string{"SCAN", "0", "MATCH", "*", "COUNT", "50"}
			if clear {
				arguments = []string{"FLUSHDB", "ASYNC"}
			}
			address := redisSteps(t, redisTestStep{[]string{"SELECT", "0"}, "+OK\r\n"}, redisTestStep{arguments, ""})
			t.Setenv("REDIS_ADDR", address)
			app := testApp(t)
			cookie, csrf := loginTest(t, app)
			started := time.Now()
			method, path, body := "GET", "keys", ""
			if clear {
				method, path, body = "POST", "clear", fmt.Sprintf(`{"confirmation":"清空 db0","address":%q}`, address)
			}
			response := requestAPI(app, method, "/api/redis/databases/db0/"+path, body, cookie, csrf)
			if response.Code != 504 || time.Since(started) > 4*time.Second {
				t.Fatal(response.Code, response.Body.String())
			}
			if clear && (!strings.Contains(response.Body.String(), "不确定") || !strings.Contains(response.Body.String(), "禁止自动重试")) {
				t.Fatal("unsafe retry wording")
			}
		})
	}
}

// TestRedisDataLimits rejects excessive nodes/wire/JSON output without returning a partial SCAN page.
func TestRedisDataLimits(t *testing.T) {
	replies := []string{"*2\r\n" + redisBulk("9") + "*8193\r\n", "*2\r\n" + redisBulk("9") + "*1\r\n$1048576\r\n", "*2\r\n" + redisBulk("9") + redisArray(strings.Repeat("\"", 400000))}
	for index, reply := range replies {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			address := redisSteps(t, redisTestStep{[]string{"SELECT", "0"}, "+OK\r\n"}, redisTestStep{[]string{"SCAN", "0", "MATCH", "*", "COUNT", "50"}, reply})
			t.Setenv("REDIS_ADDR", address)
			app := testApp(t)
			cookie, _ := loginTest(t, app)
			response := requestAPI(app, "GET", "/api/redis/databases/db0/keys", "", cookie, "")
			if response.Code != 502 {
				t.Fatal(response.Code, response.Body.String())
			}
			if response.Body.Len() > redisResponseMax {
				t.Fatal("HTTP response exceeds cap")
			}
		})
	}
	recorder := httptest.NewRecorder()
	writeRedisJSON(recorder, strings.Repeat("\x00", 200000))
	if recorder.Code != 502 || recorder.Body.Len() > redisResponseMax {
		t.Fatal("JSON escaping not bounded")
	}
}

// TestRedisRESPDepthAndIntegerBounds verifies limited nesting, noncanonical integers and null-array validation.
func TestRedisRESPDepthAndIntegerBounds(t *testing.T) {
	for _, wire := range []string{"*1\r\n*1\r\n*1\r\n*1\r\n$0\r\n\r\n", ":+1\r\n", ":9223372036854775808\r\n", "$01\r\nx\r\n"} {
		limited := &io.LimitedReader{R: strings.NewReader(wire), N: redisResponseMax}
		nodes := redisElementMax
		if _, err := readRedisRESP(bufio.NewReader(limited), limited, 0, &nodes); err == nil {
			t.Fatal("accepted invalid RESP")
		}
	}
}
