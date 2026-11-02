package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sort"
	"unicode/utf8"
)

const redisStringPreviewMax = 4096

// redisValueResult is a bounded, non-atomic preview; encoding describes value, never the keyToken.
type redisValueResult struct {
	Database   string `json:"database"`
	Key        string `json:"key"`
	KeyToken   string `json:"keyToken"`
	Type       string `json:"type"`
	TTLSeconds int64  `json:"ttlSeconds"`
	Value      string `json:"value"`
	Truncated  bool   `json:"truncated"`
	Encoding   string `json:"encoding"`
}

// redisValueAtom represents each JSON collection element with explicit UTF-8/base64 encoding.
// Using arrays of atoms/pairs preserves order and binary field identity without JSON object key loss.
type redisValueAtom struct {
	Value    string `json:"value"`
	Encoding string `json:"encoding"`
}

// redisAtom encodes raw bytes losslessly, returning UTF-8 text or padded standard base64 explicitly.
func redisAtom(raw []byte) redisValueAtom {
	if utf8.Valid(raw) {
		return redisValueAtom{string(raw), "utf8"}
	}
	return redisValueAtom{base64.StdEncoding.EncodeToString(raw), "base64"}
}

// readValue validates the canonical key token and database before selecting and loading a typed preview.
// Empty tokens are valid empty Redis keys; omission is rejected, and display text is never used to address keys.
func (monitor *redisMonitor) readValue(request *http.Request, database string) (redisValueResult, error) {
	result := redisValueResult{Database: database, Encoding: "utf8"}
	query, err := redisQuery(request, "keyToken")
	if err != nil || !query.Has("keyToken") {
		return result, errRedisInput
	}
	token := query.Get("keyToken")
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(raw) > redisResponseMax || base64.RawURLEncoding.EncodeToString(raw) != token {
		return result, errRedisInput
	}
	display := redisDisplayKey(raw)
	result.Key, result.KeyToken = display.Key, display.KeyToken
	connection, err := monitor.connect(request.Context())
	if err != nil {
		return result, err
	}
	defer connection.Close()
	if err = connection.selectDatabase(database); err != nil {
		return result, err
	}
	reply, err := connection.command("TYPE", string(raw))
	if err != nil {
		return result, err
	}
	if reply.kind != '+' {
		return result, errRedisProtocol
	}
	result.Type = string(reply.text)
	ttl, err := connection.command("TTL", string(raw))
	if err != nil {
		return result, err
	}
	if ttl.kind != ':' || ttl.integer < -2 {
		return result, errRedisProtocol
	}
	result.TTLSeconds = ttl.integer
	if result.Type == "none" || ttl.integer == -2 {
		result.Type, result.TTLSeconds, result.Value = "none", -2, "键不存在或已过期"
		return result, nil
	}
	err = connection.fillPreview(&result, string(raw))
	return result, err
}

// fillPreview dispatches a whitelist of bounded read-only commands for key, filling result or returning error.
// Streams and module-defined types explicitly report unsupported previews instead of pretending to be empty.
func (connection *redisConnection) fillPreview(result *redisValueResult, key string) error {
	switch result.Type {
	case "string":
		reply, err := connection.command("GETRANGE", key, "0", "4096")
		if err != nil {
			return err
		}
		if reply.kind != '$' || reply.null {
			return errRedisProtocol
		}
		if len(reply.text) > redisStringPreviewMax+1 {
			return errRedisProtocol
		}
		result.Truncated = len(reply.text) > redisStringPreviewMax
		if result.Truncated {
			reply.text = reply.text[:redisStringPreviewMax]
		}
		atom := redisAtom(reply.text)
		result.Value, result.Encoding = atom.Value, atom.Encoding
		return nil
	case "hash", "set", "list", "zset":
		return connection.collectionPreview(result, key)
	case "stream":
		result.Value = "暂不支持 stream 类型预览"
		return nil
	default:
		result.Value = "暂不支持此 Redis 类型预览"
		return nil
	}
}

// collectionPreview obtains bounded ordered JSON previews and evidence-based truncation metadata.
// HSCAN/SSCAN COUNT is a hint; all returned elements are retained or the entire request errors.
func (connection *redisConnection) collectionPreview(result *redisValueResult, key string) error {
	var reply redisRESP
	var err error
	switch result.Type {
	case "hash":
		reply, err = connection.command("HSCAN", key, "0", "COUNT", "20")
	case "set":
		reply, err = connection.command("SSCAN", key, "0", "COUNT", "20")
	case "list":
		reply, err = connection.command("LRANGE", key, "0", "19")
	case "zset":
		reply, err = connection.command("ZRANGE", key, "0", "19", "WITHSCORES")
	}
	if err != nil {
		return err
	}
	items := reply.items
	if result.Type == "hash" || result.Type == "set" {
		var cursor string
		cursor, items, err = redisScanParts(reply)
		if err != nil {
			return err
		}
		result.Truncated = cursor != "0"
	} else {
		if err = redisBulkArray(reply); err != nil {
			return err
		}
		command := "LLEN"
		returned := len(items)
		if result.Type == "zset" {
			command, returned = "ZCARD", returned/2
		}
		if returned > 20 {
			return errRedisProtocol
		}
		length, lengthErr := connection.command(command, key)
		if lengthErr != nil {
			return lengthErr
		}
		if length.kind != ':' || length.integer < 0 {
			return errRedisProtocol
		}
		result.Truncated = length.integer > int64(returned)
	}
	encoded, err := encodeRedisCollection(result.Type, items)
	if err != nil {
		return err
	}
	result.Value, result.Encoding = string(encoded), "json"
	return nil
}

// encodeRedisCollection returns deterministic JSON arrays of atoms or field/value and member/score pairs.
// Sets and hashes sort by raw bytes; lists and sorted sets preserve Redis order.
func encodeRedisCollection(kind string, items []redisRESP) ([]byte, error) {
	if kind == "hash" || kind == "zset" {
		if len(items)%2 != 0 {
			return nil, errRedisProtocol
		}
		pairs := make([][2]redisRESP, 0, len(items)/2)
		for index := 0; index < len(items); index += 2 {
			pairs = append(pairs, [2]redisRESP{items[index], items[index+1]})
		}
		if kind == "hash" {
			sort.SliceStable(pairs, func(left, right int) bool { return string(pairs[left][0].text) < string(pairs[right][0].text) })
		}
		values := make([][2]redisValueAtom, 0, len(pairs))
		for _, pair := range pairs {
			values = append(values, [2]redisValueAtom{redisAtom(pair[0].text), redisAtom(pair[1].text)})
		}
		return json.Marshal(values)
	}
	if kind == "set" {
		sort.SliceStable(items, func(left, right int) bool { return string(items[left].text) < string(items[right].text) })
	}
	values := make([]redisValueAtom, 0, len(items))
	for _, item := range items {
		values = append(values, redisAtom(item.text))
	}
	return json.Marshal(values)
}
