#!/bin/sh
# Usage: sh deploy/host-network-migrate.sh PROJECT_DIR LAN_IPV4 [check|migrate]
# Migrates the verified initial Ubuntu bridge deployment; never touches OpenWrt.
# New image must be built first. Redis is re-created once to add LAN publication.
set -eu
umask 077

# fail(message): emit a non-secret diagnostic and stop without deleting data.
fail() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
[ "$#" -ge 2 ] && [ "$#" -le 3 ] || fail 'usage: host-network-migrate.sh PROJECT_DIR LAN_IPV4 [check|migrate]'
project=$1
address=$2
mode=${3:-check}
case "$mode" in check|migrate) ;; *) fail 'mode must be check or migrate';; esac
HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
sh "$HERE/host-network-check.sh" "$project" "$address"
cd "$project"
[ -f compose.ubuntu.yaml ] || fail 'compose.ubuntu.yaml missing'
[ -f compose.bind.yaml ] && [ -f data/compose.vars ] || fail 'original deployment files missing'
for directory in data/updater data/updater/run; do
    [ ! -L "$directory" ] || fail 'updater directory must not be a symlink'
done
[ ! -e data/ubuntu.vars ] && [ ! -L data/ubuntu.vars ] || fail 'Ubuntu migration already prepared; inspect before retrying'

# old_compose(args): target the original, explicitly named project on the local daemon.
old_compose() {
    docker --host unix:///var/run/docker.sock compose --env-file data/compose.vars \
        -p db-relay -f compose.yaml -f compose.bind.yaml "$@"
}
# new_compose(args): use only the standalone host-network manifest and fixed variables.
new_compose() {
    docker --host unix:///var/run/docker.sock compose --env-file data/ubuntu.vars \
        -p db-relay -f compose.ubuntu.yaml "$@"
}
relay_id=$(old_compose ps -q db-relay)
network=$(docker --host unix:///var/run/docker.sock inspect --format '{{.HostConfig.NetworkMode}}' "$relay_id")
[ "$network" != host ] || fail 'relay already uses host networking'
# New Redis LAN publication must not steal an existing host listener.
if ss -H -lnt '( sport = :6379 )' | awk 'NF {found=1} END {exit !found}'; then
    fail 'host TCP 6379 is occupied; stop and resolve the conflict'
fi
old_compose config --quiet
[ "$mode" = migrate ] || { printf '\nRead-only checks passed; no changes made.\n'; exit 0; }
[ "$(id -u)" = 0 ] || fail 'migration requires root'
image_meta=$(docker --host unix:///var/run/docker.sock image inspect --format '{{.Id}}|{{index .Config.Labels "io.db-relay.network.host"}}|{{.Os}}/{{.Architecture}}' local/db-relay:ubuntu) || fail 'build local/db-relay:ubuntu first'
case "$image_meta" in sha256:*'|v1|linux/amd64') ;; *) fail 'image lacks the Ubuntu host-network contract';; esac
image_id=${image_meta%%|*}
# Preserve the old image even if a later build reuses its tag.
old_image=$(docker --host unix:///var/run/docker.sock inspect --format '{{.Image}}' "$relay_id")
backup=$(mktemp -d "$project/data/migration/host-switch.XXXXXX")
docker --host unix:///var/run/docker.sock image tag "$old_image" "local/db-relay:before-host-$(basename "$backup")"
cp -p compose.yaml compose.bind.yaml data/compose.vars "$backup/"
printf '%s\n' "$old_image" > "$backup/old-image-id"
install -d -m 700 data/updater
install -d -m 750 -o 0 -g 10001 data/updater/run
printf '%s\n' "LAN_BIND_IP=$address" "RELAY_IMAGE=$image_id" 'ONLINE_UPDATE_SOCKET=' > data/ubuntu.vars
new_compose config --quiet

printf '\nStopping relay briefly to take a consistent configuration snapshot.\n'
old_compose stop db-relay
if ! tar -czf "$backup/relay-data.tar.gz" -C data relay secrets; then
    printf 'Backup failed; restarting the original relay.\n' >&2
    old_compose start db-relay || fail 'original relay restart failed; inspect manually'
    exit 1
fi
printf '\nStarting host-network relay; Redis data stays in the same directory.\n'
if ! new_compose up -d --no-build --pull never --wait --wait-timeout 120; then
    printf 'New deployment did not become healthy. Restoring original networking.\n' >&2
    new_compose stop db-relay redis-dev || fail 'cannot stop new containers; manual recovery required'
    old_compose up -d --no-build --pull never --wait --wait-timeout 120 || fail 'original deployment recovery failed; retain all backup files'
    printf 'Original deployment restored; migration files retained at %s. Do not rerun blindly.\n' "$backup"
    exit 1
fi
new_compose ps
printf '\nHost migration complete. Backup: %s\n' "$backup"
printf 'Management: http://%s:18080/\n' "$address"
printf 'Verify trusted LAN firewall rules and actual database connectivity before retiring the old router.\n'
printf 'Online update service installation is a separate explicit step; check-only mode remains available.\n'
