#!/bin/sh
# Usage (manual on OpenWrt only):
# LAN_INTERFACE=br-lan LAN_BIND_IP=192.168.10.1 LAN_INTERFACE_CONFIRMED=yes \
# REDIS_LAN_ACCESS_CONFIRMED=yes sh redis-init.sh [check|start|restart-migrate]
# Default check is read-only. start refuses ANY existing service directory or
# container. Interrupted initialization requires manual review, not auto deletion.
# No automatic pulls. Import redis:7.4.7-alpine linux/amd64 beforehand.
# Redis RDB is periodic, not per-write durability. 256MB can still OOM during fork.
# Docker port publication may itself alter Docker-managed NAT rules; this script
# never executes firewall commands or restarts Docker. Binding is not a source ACL.
set -eu
HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
. "$HERE/common.sh"
MODE=${1:-check}
[ "$#" -le 1 ] || fail 'usage: redis-init.sh [check|start|restart-migrate]'
case "$MODE" in check|start|restart-migrate) ;; *) fail 'usage: redis-init.sh [check|start|restart-migrate]';; esac
usb_check
lan_check
[ "${REDIS_LAN_ACCESS_CONFIRMED:-}" = yes ] || fail 'set REDIS_LAN_ACCESS_CONFIRMED=yes: unauthenticated LAN access is NOT source-isolated'
# redis_restart_migrate(): validate exact existing Redis layout under its deployment lock.
# Explicit policy migration never starts/stops Redis and preserves manual stop semantics.
redis_restart_migrate() {
    private_dir "$REDIS" 0
    acquire_lock "$REDIS"
    trap release_lock EXIT
    [ "$(docker_local inspect --format "{{index .Config.Labels \"$LABEL\"}}" redis-dev)" = redis-v1 ] || fail 'unmanaged Redis'
    redis_id=$(docker_local inspect --format '{{.Id}}' redis-dev)
    case "$redis_id" in ''|*[!0-9a-f]*) fail 'invalid Redis identity';; esac
    [ "${#redis_id}" = 64 ] || fail 'full Redis identity required'
    [ "$(docker_local inspect --format '{{.Image}}' "$redis_id")" = "$IMAGE_ID" ] || fail 'Redis image mismatch'
    case "$(docker_local inspect --format '{{.Config.User}} {{.HostConfig.RestartPolicy.Name}}' "$redis_id")" in
        '999:999 no'|'999:999 unless-stopped') ;; *) fail 'unsupported Redis user/restart policy';;
    esac
    expected=$(printf '%s\n' "$REDIS/data|/data|true" "$REDIS/config/redis.conf|/usr/local/etc/redis/redis.conf|false" | LC_ALL=C sort)
    actual=$(docker_local inspect --format '{{range .Mounts}}{{if eq .Type "bind"}}{{printf "%s|%s|%t\n" .Source .Destination .RW}}{{end}}{{end}}' "$redis_id" | LC_ALL=C sort)
    [ "$actual" = "$expected" ] || fail 'unsupported Redis mounts'
    extra=$(docker_local inspect --format '{{range .Mounts}}{{if ne .Type "bind"}}{{.Type}}:{{.Destination}};{{end}}{{end}}' "$redis_id")
    case "$extra" in ''|'tmpfs:/tmp;') ;; *) fail 'unsupported Redis non-bind mount';; esac
    [ "$(docker_local inspect --format '{{range $port, $bindings := .HostConfig.PortBindings}}{{range $bindings}}{{.HostIp}}:{{.HostPort}}:{{$port}};{{end}}{{end}}' "$redis_id")" = "$LAN_BIND_IP:6379:6379/tcp;" ] || fail 'unsupported Redis ports'
    profile=$(docker_local inspect --format '{{.HostConfig.Privileged}}|{{.HostConfig.ReadonlyRootfs}}|{{.HostConfig.NetworkMode}}|{{.HostConfig.NanoCpus}}|{{.HostConfig.Memory}}|{{.HostConfig.MemorySwap}}|{{.HostConfig.PidsLimit}}|{{json .HostConfig.CapDrop}}|{{json .HostConfig.SecurityOpt}}|{{json .HostConfig.Devices}}|{{.HostConfig.PidMode}}|{{json .HostConfig.CapAdd}}' "$redis_id")
    case "$profile" in
        'false|true|default|500000000|268435456|268435456|64|["ALL"]|["no-new-privileges:true"]|[]||null'|'false|true|default|500000000|268435456|268435456|64|["ALL"]|["no-new-privileges:true"]|[]||[]') ;;
        *) fail 'unsupported Redis security/resource/network layout';;
    esac
    private_dir "$REDIS/data" 999
    safe_path "$REDIS/config/redis.conf"
    [ -f "$REDIS/config/redis.conf" ] || fail 'missing Redis configuration'
    log 'setting verified Redis restart policy; running/stopped state unchanged'
    docker_local update --restart unless-stopped "$redis_id" >/dev/null
}
image_check redis:7.4.7-alpine
if [ "$MODE" = restart-migrate ]; then redis_restart_migrate; exit 0; fi
require_absent redis-dev
safe_path "$REDIS"
[ ! -e "$REDIS" ] || fail 'existing Redis directory: refusing to overwrite config/data/ownership'
safe_path "$HERE/redis.conf"
[ -f "$HERE/redis.conf" ] || fail 'missing Redis configuration'
log 'preflight passed; no writes performed; unauthenticated Redis requires trusted LAN'
[ "$MODE" = start ] || exit 0
# Atomic top-level mkdir also excludes concurrent initialization; no recursive chown.
new_dir "$REDIS" 0
new_dir "$REDIS/data" 999
new_dir "$REDIS/config" 0
# UID 999 must traverse config directory, but only root may modify it.
chmod 755 "$REDIS/config"
cp "$HERE/redis.conf" "$REDIS/config/redis.conf"
chmod 444 "$REDIS/config/redis.conf"
log 'starting independent Redis container; existing data is never removed on failure'
docker_local run -d --pull never --name redis-dev --label "$LABEL=redis-v1" \
  --restart unless-stopped --user 999:999 --read-only --cap-drop ALL \
  --security-opt no-new-privileges:true --cpus 0.5 --memory 256m --memory-swap 256m --pids-limit 64 \
  --log-driver json-file --log-opt max-size=2m --log-opt max-file=2 \
  --tmpfs /tmp:rw,noexec,nosuid,size=8m --stop-timeout 30 \
  --mount "type=bind,src=$REDIS/data,dst=/data" \
  --mount "type=bind,src=$REDIS/config/redis.conf,dst=/usr/local/etc/redis/redis.conf,readonly" \
  -p "$LAN_BIND_IP:6379:6379/tcp" --health-cmd 'redis-cli ping | grep -qx PONG' \
  --health-interval 10s --health-timeout 3s --health-retries 3 \
  "$IMAGE_ID" redis-server /usr/local/etc/redis/redis.conf >/dev/null
log 'Redis container created; inspect health manually; no router validation claimed'
