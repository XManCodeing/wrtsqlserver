package main

import (
	"math"
	"sort"
	"strconv"
	"strings"
)

// parseRedisInfo parses bounded INFO text and projects only approved section/field pairs.
// Missing fields remain nil; a returned but empty Keyspace section means zero total keys.
func parseRedisInfo(info string) (*redisMetrics, []redisDatabase, error) {
	metrics := &redisMetrics{}
	databases := []redisDatabase{}
	section := ""
	seenSections := map[string]bool{}
	seenFields := map[string]bool{}
	fieldCount := 0
	for _, line := range strings.Split(info, "\r\n") {
		if line == "" {
			continue
		}
		if strings.ContainsAny(line, "\r\n\x00") {
			return nil, nil, errRedisProtocol
		}
		if strings.HasPrefix(line, "# ") {
			section = strings.ToLower(strings.TrimPrefix(line, "# "))
			if seenSections[section] || section == "" {
				return nil, nil, errRedisProtocol
			}
			seenSections[section] = true
			continue
		}
		key, value, found := strings.Cut(line, ":")
		if !found || key == "" || section == "" || seenFields[section+":"+key] {
			return nil, nil, errRedisProtocol
		}
		seenFields[section+":"+key] = true
		if section == "keyspace" {
			database, err := parseRedisDatabase(key, value)
			if err != nil {
				return nil, nil, err
			}
			databases = append(databases, database)
		} else if err := setRedisMetric(metrics, section+":"+key, value); err != nil {
			return nil, nil, err
		}
		fieldCount++
	}
	knownSection := seenSections["server"] || seenSections["memory"] || seenSections["clients"] || seenSections["stats"] || seenSections["persistence"] || seenSections["keyspace"]
	if !knownSection || (fieldCount == 0 && !seenSections["keyspace"]) {
		return nil, nil, errRedisProtocol
	}
	if seenSections["keyspace"] {
		total := int64(0)
		for _, database := range databases {
			if database.Keys > math.MaxInt64-total {
				return nil, nil, errRedisProtocol
			}
			total += database.Keys
		}
		metrics.TotalKeys = &total
	}
	sort.Slice(databases, func(left, right int) bool { return databases[left].Name < databases[right].Name })
	return metrics, databases, nil
}

// setRedisMetric assigns a single whitelisted field to metrics; unknown fields are intentionally discarded.
// Malformed known fields fail the snapshot rather than becoming misleading zero values.
func setRedisMetric(metrics *redisMetrics, field, value string) error {
	var number **int64
	switch field {
	case "server:redis_version":
		if value == "" || len(value) > 64 || strings.Trim(value, "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ.-_") != "" {
			return errRedisProtocol
		}
		metrics.Version = &value
	case "server:uptime_in_seconds":
		number = &metrics.UptimeSeconds
	case "memory:used_memory":
		number = &metrics.UsedMemoryBytes
	case "memory:maxmemory":
		number = &metrics.MaxMemoryBytes
	case "clients:connected_clients":
		number = &metrics.ConnectedClients
	case "stats:instantaneous_ops_per_sec":
		number = &metrics.OpsPerSecond
	case "persistence:rdb_last_save_time":
		number = &metrics.RDBLastSaveTime
	case "persistence:rdb_changes_since_last_save":
		number = &metrics.RDBChangesSinceLastSave
	case "persistence:rdb_last_bgsave_status":
		if value != "ok" && value != "err" {
			return errRedisProtocol
		}
		metrics.RDBLastBgsaveStatus = &value
	case "persistence:rdb_bgsave_in_progress", "persistence:aof_enabled":
		if value != "0" && value != "1" {
			return errRedisProtocol
		}
		enabled := value == "1"
		if field == "persistence:aof_enabled" {
			metrics.AOFEnabled = &enabled
		} else {
			metrics.RDBBgsaveInProgress = &enabled
		}
	}
	if number != nil {
		parsed, err := redisNonnegative(value)
		if err != nil {
			return err
		}
		*number = &parsed
	}
	return nil
}

// redisNonnegative parses decimal counters with no signs or whitespace, returning a protocol error on overflow.
func redisNonnegative(value string) (int64, error) {
	if value == "" || strings.Trim(value, "0123456789") != "" {
		return 0, errRedisProtocol
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, errRedisProtocol
	}
	return parsed, nil
}

// parseRedisDatabase validates one dbN record, requiring explicit keys and expires counters.
// Optional Redis-version-specific counters are ignored, not returned.
func parseRedisDatabase(name, value string) (redisDatabase, error) {
	database := redisDatabase{Name: name}
	if !strings.HasPrefix(name, "db") {
		return database, errRedisProtocol
	}
	if _, err := redisNonnegative(strings.TrimPrefix(name, "db")); err != nil {
		return database, err
	}
	seen := map[string]bool{}
	for _, pair := range strings.Split(value, ",") {
		key, counter, found := strings.Cut(pair, "=")
		if !found || key == "" || seen[key] {
			return database, errRedisProtocol
		}
		seen[key] = true
		if key != "keys" && key != "expires" {
			continue
		}
		parsed, err := redisNonnegative(counter)
		if err != nil {
			return database, err
		}
		if key == "keys" {
			database.Keys = parsed
		} else {
			database.Expires = parsed
		}
	}
	if !seen["keys"] || !seen["expires"] || database.Expires > database.Keys {
		return database, errRedisProtocol
	}
	return database, nil
}
