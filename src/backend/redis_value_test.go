package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRedisValueCollectionLimits rejects oversized/malformed collection replies rather than claiming full previews.
func TestRedisValueCollectionLimits(t *testing.T) {
	for _, reply := range []string{
		"*2\r\n" + redisBulk("0") + "*8193\r\n",
		"*2\r\n" + redisBulk("0") + "*2\r\n" + redisBulk("field") + "$1048576\r\n",
		"*2\r\n" + redisBulk("0") + redisArray("unpaired-field"),
	} {
		address := redisSteps(t,
			redisTestStep{[]string{"SELECT", "0"}, "+OK\r\n"},
			redisTestStep{[]string{"TYPE", "key"}, "+hash\r\n"},
			redisTestStep{[]string{"TTL", "key"}, ":-1\r\n"},
			redisTestStep{[]string{"HSCAN", "key", "0", "COUNT", "20"}, reply})
		t.Setenv("REDIS_ADDR", address)
		app := testApp(t)
		cookie, _ := loginTest(t, app)
		response := requestAPI(app, "GET", "/api/redis/databases/db0/value?keyToken=a2V5", "", cookie, "")
		if response.Code != 502 || strings.Contains(response.Body.String(), `"truncated":false`) {
			t.Fatal(response.Code, response.Body.String())
		}
	}
}

// TestRedisEmptyKeyAndUTF8Boundary preserves empty keys and bytes split by the exact 4096-byte preview boundary.
func TestRedisEmptyKeyAndUTF8Boundary(t *testing.T) {
	address := redisSteps(t,
		redisTestStep{[]string{"SELECT", "0"}, "+OK\r\n"},
		redisTestStep{[]string{"TYPE", ""}, "+string\r\n"},
		redisTestStep{[]string{"TTL", ""}, ":-1\r\n"},
		redisTestStep{[]string{"GETRANGE", "", "0", "4096"}, redisBulk(strings.Repeat("x", 4095) + "\xe4\xb8")})
	t.Setenv("REDIS_ADDR", address)
	app := testApp(t)
	cookie, _ := loginTest(t, app)
	response := requestAPI(app, "GET", "/api/redis/databases/db0/value?keyToken=", "", cookie, "")
	var result redisValueResult
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil {
		t.Fatal(response.Code, response.Body.String())
	}
	if !result.Truncated || result.Encoding != "base64" || result.KeyToken != "" {
		t.Fatal("UTF-8 boundary bytes lost", result.Encoding)
	}
}

// TestRedisScanEmptyNonterminalPage preserves the cursor even when an intermediate SCAN page contains no matches.
func TestRedisScanEmptyNonterminalPage(t *testing.T) {
	address := redisSteps(t,
		redisTestStep{[]string{"SELECT", "0"}, "+OK\r\n"},
		redisTestStep{[]string{"SCAN", "42", "MATCH", "missing*", "COUNT", "50"}, "*2\r\n" + redisBulk("84") + redisArray()})
	t.Setenv("REDIS_ADDR", address)
	app := testApp(t)
	cookie, _ := loginTest(t, app)
	response := requestAPI(app, "GET", "/api/redis/databases/db0/keys?cursor=42&pattern=missing*", "", cookie, "")
	var result redisKeysResult
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil {
		t.Fatal(response.Code, response.Body.String())
	}
	if result.NextCursor != "84" || result.Keys == nil || len(result.Keys) != 0 {
		t.Fatal("empty SCAN page lost continuation")
	}
}
