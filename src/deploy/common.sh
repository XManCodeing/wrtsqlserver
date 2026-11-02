#!/bin/sh
# Shared OpenWrt/BusyBox helpers. Source only from the adjacent deployment scripts.
# No mount, daemon restart, firewall commands, image pulls, or global cleanup.
set -eu
umask 077
USB=/mnt/docker-usb
UUID=d7f45318-dc5b-49db-8250-defb536790c6
RELAY=$USB/db-relay
REDIS=$USB/redis-dev
LABEL=io.db-relay.deploy.managed
NAME=db-relay
OLD=db-relay-rollback

# fail(message): report a non-secret diagnostic; exits unsuccessfully.
fail() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
# log(message): emit an operational event without credentials; returns printf status.
log() { printf '%s\n' "$*" >&2; }
# docker_local(arguments): always target the local socket, never a remote context.
docker_local() { command docker --host unix:///var/run/docker.sock "$@"; }
# file_stat(format arguments): use host stat or the adjacent read-only helper; never install globally.
file_stat() {
    if command -v stat >/dev/null 2>&1; then
        command stat "$@"
    elif [ -n "${HERE:-}" ] && [ ! -L "$HERE/deploy-stat" ] && [ -f "$HERE/deploy-stat" ] && [ -x "$HERE/deploy-stat" ]; then
        "$HERE/deploy-stat" "$@"
    else
        fail 'stat unavailable: include the linux/amd64 deploy-stat helper beside these scripts'
    fi
}
# docker_mount_record(): validate mountinfo on stdin; accept only the exact USB /docker self-bind.
# Requires one USB root record and one direct child record, same device/source, ext4 and rw.
docker_mount_record() {
    awk -v usb="$USB" '
      function writable(options) { return index("," options ",", ",rw,") > 0 }
      $5==usb || $5==usb "/docker" {
        separator=0
        for(i=7;i<=NF;i++) if($i=="-") {separator=i; break}
        if(!separator || $(separator+1)!="ext4" || !writable($6) || !writable($(separator+3))) bad=1
        if($5==usb) {
          roots++; rootid=$1; device=$3; source=$(separator+2)
          if($4!="/") bad=1
        } else {
          children++; parent=$2; childdevice=$3; childsource=$(separator+2)
          if($4!="/docker") bad=1
        }
      }
      END {exit (bad || roots!=1 || children!=1 || parent!=rootid || device!=childdevice || source!=childsource)}
    '
}
# safe_path(absolute_path): reject symlinks in every existing component; returns 0.
safe_path() {
    case "$1" in /*) ;; *) fail 'absolute path required';; esac
    path_part=$1
    while [ "$path_part" != / ]; do
        [ ! -L "$path_part" ] || fail "symlink rejected: $path_part"
        case "$path_part" in
            "$USB"/*)
                if [ -e "$path_part" ]; then
                    [ "$(file_stat -c %d "$path_part")" = "$(file_stat -c %d "$USB")" ] || fail "path leaves USB filesystem: $path_part"
                    if mountpoint -q "$path_part"; then
                        [ "$path_part" = "$USB/docker" ] && [ "$1" = "$USB/docker" ] || fail "nested mount rejected: $path_part"
                        docker_mount_record < /proc/self/mountinfo || fail 'unverified Docker self-bind mount'
                    fi
                fi
                ;;
        esac
        path_part=${path_part%/*}
        [ -n "$path_part" ] || path_part=/
    done
}
# private_dir(path, uid): require an existing 0700 directory owned by uid; returns 0.
private_dir() {
    safe_path "$1"
    [ -d "$1" ] || fail "missing directory: $1"
    [ "$(file_stat -c '%u:%a' "$1")" = "$2:700" ] || fail "unsafe ownership/mode: $1"
}
# private_file(path, uid): reject special/hardlinked files and wrong mode; returns 0.
private_file() {
    safe_path "$1"
    [ -f "$1" ] || fail "missing regular file: $1"
    [ "$(file_stat -c '%u:%a:%h' "$1")" = "$2:600:1" ] || fail "unsafe file: $1"
}
# usb_check(): read-only UUID, actual mount, rw and Docker-root verification.
usb_check() {
    [ "$(id -u)" = 0 ] || fail 'run manually as root on the OpenWrt host'
    for required in docker ip awk mountpoint readlink block; do
        command -v "$required" >/dev/null || fail "missing command: $required"
    done
    safe_path "$USB"
    mountpoint -q "$USB" || fail 'USB is not a mountpoint'
    mount_record=$(awk -v target="$USB" '$2==target {print $1 " " $3 " " $4; count++} END {if(count!=1) exit 1}' /proc/mounts) || fail 'ambiguous/missing USB mount'
    set -- $mount_record
    [ "$2" = ext4 ] || fail 'USB must be ext4'
    case ",$3," in *,rw,*) ;; *) fail 'USB is not rw';; esac
    mounted_device=$(readlink -f "$1")
    [ -b "$mounted_device" ] || fail 'USB source is not a block device'
    block info "$mounted_device" | awk -v uuid="$UUID" 'index($0,"UUID=\"" uuid "\"") {ok=1} END {exit !ok}' || fail 'USB UUID mismatch'
    [ "$(file_stat -c %u "$USB")" = 0 ] || fail 'USB mount must be root owned'
    case "$(file_stat -c %a "$USB")" in 700|750|755) ;; *) fail 'unsafe USB mount permissions';; esac
    safe_path "$USB/docker"
    [ "$(docker_local info --format '{{.DockerRootDir}}')" = "$USB/docker" ] || fail 'Docker root must be on the verified USB'
}
# lan_check(): validate explicit RFC1918 address on an explicitly selected LAN interface.
# Caller must confirm interface is LAN: RFC1918 alone cannot identify private WANs.
lan_check() {
    [ "${LAN_INTERFACE_CONFIRMED:-}" = yes ] || fail 'set LAN_INTERFACE_CONFIRMED=yes after confirming the interface is LAN, not WAN'
    [ -n "${LAN_INTERFACE:-}" ] && [ -n "${LAN_BIND_IP:-}" ] || fail 'set LAN_INTERFACE and LAN_BIND_IP'
    case "$LAN_INTERFACE" in *[!a-zA-Z0-9_.:-]*|-*) fail 'invalid LAN interface';; esac
    printf '%s\n' "$LAN_BIND_IP" | awk -F. '
      NF!=4 {exit 1}
      {for(i=1;i<=4;i++) if($i !~ /^[0-9]+$/ || length($i)>3 || $i>255 || (length($i)>1 && substr($i,1,1)=="0")) exit 1}
      !($1==10 || ($1==172 && $2>=16 && $2<=31) || ($1==192 && $2==168)) {exit 1}
    ' || fail 'LAN_BIND_IP must be canonical RFC1918 IPv4'
    ip -4 addr show dev "$LAN_INTERFACE" | awk -v wanted="$LAN_BIND_IP" '$1=="inet" {split($2,ip,"/"); if(ip[1]==wanted) ok=1} END {exit !ok}' || fail 'LAN address is not assigned to selected local interface'
}
# image_check(tag): resolve an existing explicit tag to immutable linux/amd64 image ID.
image_check() {
    case "$1" in *:latest|*[!a-zA-Z0-9_./:-]*|:*|*:) fail 'explicit non-latest local image tag required';; *:*) ;; *) fail 'image tag required';; esac
    image_meta=$(docker_local image inspect --format '{{.Os}}/{{.Architecture}} {{.Id}}' "$1") || fail 'image missing locally; import it manually, no automatic pull'
    [ "${image_meta%% *}" = linux/amd64 ] || fail 'image must be linux/amd64'
    IMAGE_ID=${image_meta#* }
    case "$IMAGE_ID" in sha256:*) ;; *) fail 'invalid image ID';; esac
}
# container_id(name): prints exact named container ID, empty if absent; propagates daemon errors.
container_id() {
    docker_local container ls -a --filter "name=^/$1$" --format '{{.ID}}'
}
# require_absent(name): refuses existing containers, including stopped ones.
require_absent() {
    found_id=$(container_id "$1") || fail 'cannot list containers'
    [ -z "$found_id" ] || fail "container already exists: $1"
}
# managed(name): reject containers not explicitly owned by these scripts.
managed() {
    [ "$(docker_local inspect --format "{{index .Config.Labels \"$LABEL\"}}" "$1")" = relay-v1 ] || fail "unmanaged container: $1"
}
# new_dir(path, uid): create ONLY a nonexistent exact directory, no recursive chown.
new_dir() {
    safe_path "$1"
    [ ! -e "$1" ] || fail "refusing existing path: $1"
    mkdir -m 700 "$1"
    chown "$2:$2" "$1"
}
# acquire_lock(root): serialize cooperating scripts; stale locks require manual confirmation.
acquire_lock() {
    LOCK=$1/.deploy-lock
    safe_path "$LOCK"
    if [ -d "$LOCK" ]; then
        private_dir "$LOCK" 0
        private_file "$LOCK/pid" 0
        lock_pid=$(cat "$LOCK/pid")
        case "$lock_pid" in ''|*[!0-9]*) fail 'invalid lock PID; manual investigation required';; esac
        if kill -0 "$lock_pid" 2>/dev/null; then fail 'another deployment is active'; fi
        [ "${STALE_LOCK_CONFIRMED:-}" = yes ] || fail 'stale lock: investigate journal, then explicitly set STALE_LOCK_CONFIRMED=yes'
        rm "$LOCK/pid"
        rmdir "$LOCK"
    fi
    mkdir -m 700 "$LOCK" || fail 'deployment lock unavailable'
    printf '%s\n' "$$" > "$LOCK/pid"
}
# release_lock(): remove only our exact lock; never recursively delete anything.
release_lock() { rm "$LOCK/pid" && rmdir "$LOCK"; }
# updater_run_check(require_socket): validate the root-owned USB control directory.
# Argument yes requires a live socket inode; no permits absence before daemon start.
# Returns success only for exact permissions and no symlink/nested-mount escapes.
updater_run_check() {
    private_dir "$USB/relay-updater" 0
    safe_path "$USB/relay-updater/run"
    [ -d "$USB/relay-updater/run" ] || fail 'updater run directory missing'
    [ "$(file_stat -c '%u:%g:%a' "$USB/relay-updater/run")" = '0:10001:750' ] || fail 'unsafe updater run directory'
    safe_path "$USB/relay-updater/run/control.sock"
    if [ -e "$USB/relay-updater/run/control.sock" ]; then
        [ -S "$USB/relay-updater/run/control.sock" ] || fail 'updater control endpoint is not a socket'
        [ "$(file_stat -c '%u:%g:%a' "$USB/relay-updater/run/control.sock")" = '0:10001:660' ] || fail 'unsafe updater socket'
    else
        [ "$1" = no ] || fail 'updater control socket missing'
    fi
}
# relay_run(image_id): create fixed managed relay, using established data and password.
# Optional online control is enabled only after validating the host socket directory.
relay_run() {
    relay_image=$1
    case "${RELAY_RESTART_POLICY:-unless-stopped}" in no|unless-stopped) ;; *) fail 'unsupported relay restart policy';; esac
    set --
    if [ -e "$USB/relay-updater/run/control.sock" ] || [ -L "$USB/relay-updater/run/control.sock" ]; then
        updater_run_check yes
        set -- --mount "type=bind,src=$USB/relay-updater/run,dst=/run/relay-updater,readonly" \
          -e ONLINE_UPDATE_SOCKET=/run/relay-updater/control.sock
    elif [ "${REQUIRE_ONLINE_UPDATE:-no}" = yes ]; then
        fail 'existing online update capability requires a safe updater socket'
    fi
    for relay_port in $(printf '%s' "${RELAY_PORTS-13306}" | tr ',' ' '); do
        set -- "$@" -p "$LAN_BIND_IP:$relay_port:$relay_port/tcp"
    done
    docker_local run -d --pull never --name "$NAME" \
      --label "$LABEL=relay-v1" --label "io.db-relay.deploy.transaction=${TRANSACTION:-initial}" \
      --restart "${RELAY_RESTART_POLICY:-unless-stopped}" --user 10001:10001 --read-only --cap-drop ALL \
      --security-opt no-new-privileges:true --cpus 0.5 --memory 128m --memory-swap 128m --pids-limit 64 \
      --log-driver json-file --log-opt max-size=2m --log-opt max-file=2 \
      --tmpfs /tmp:rw,noexec,nosuid,size=8m --stop-timeout 15 \
      --mount "type=bind,src=$RELAY/data,dst=/data" \
      --mount "type=bind,src=$RELAY/admin_password,dst=/run/secrets/admin_password,readonly" \
      -e LISTEN_ADDR=0.0.0.0:8080 -e APP_DATA_DIR=/data -e WEB_DIR=/app/web \
      -e ADMIN_PASSWORD_FILE=/run/secrets/admin_password -e MAX_CONNECTIONS=128 \
      -e "REDIS_ADDR=$LAN_BIND_IP:6379" \
      -p "$LAN_BIND_IP:18080:8080/tcp" \
      --health-cmd 'wget -q -T 3 -O - http://127.0.0.1:8080/api/session | grep -q '\''"authenticated":false'\''' \
      --health-interval 5s --health-timeout 4s --health-retries 6 "$@" "$relay_image" >/dev/null
}
# relay_health(): require Docker health plus HTTP session response; bounded 40s poll.
relay_health() {
    health_attempt=0
    while [ "$health_attempt" -lt 20 ]; do
        if [ "$(docker_local inspect --format '{{.State.Running}} {{.State.Health.Status}}' "$NAME")" = 'true healthy' ]; then
            session_reply=$(wget -q -T 3 -O - "http://$LAN_BIND_IP:18080/api/session") || return 1
            [ "$session_reply" = '{"authenticated":false}' ] && return 0
        fi
        health_attempt=$((health_attempt + 1))
        sleep 2
    done
    log 'relay health/session check failed'
    return 1
}
