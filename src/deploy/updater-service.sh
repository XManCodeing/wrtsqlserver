#!/bin/sh
# Explicit procd installer. Default check never changes the host. Upgrade preserves enable state.
# Cross-directory upgrade: PREVIOUS_BUNDLE=/mnt/docker-usb/relay-updater/deploy-1.2.0 \
# sh /mnt/docker-usb/relay-updater/deploy-1.2.1/updater-service.sh upgrade
# A stopped/unhealthy active relay keeps the boot worker waiting until bounded timeout;
# it never starts the relay, replays a transaction or registers an infinite respawn.
set -eu
HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
. "$HERE/common.sh"
MODE=${1:-check}
[ "$#" -le 1 ] || fail 'one operation argument required'
case "$MODE" in check|install|upgrade|enable|start|run) ;; *) fail 'usage: updater-service.sh [check|install|upgrade|enable|start|run]';; esac
INIT=/etc/init.d/relay-updater

# service_check(): inspect verified USB, trusted bundle and prerequisites without mutations.
service_check() {
    usb_check
    private_file "$USB/relay-updater/config.json" 0
    safe_path "$HERE/relay-updater"
    [ -x "$HERE/relay-updater" ] || fail 'packaged updater binary missing'
    case "$(file_stat -c '%u:%a:%h' "$HERE/relay-updater")" in 0:700:1|0:750:1|0:755:1) ;; *) fail 'untrusted updater executable';; esac
    service_path=$HERE
    while [ "$service_path" != / ]; do
        [ "$(file_stat -c %u "$service_path")" = 0 ] || fail 'service path must be root owned'
        case "$(file_stat -c %a "$service_path")" in 700|750|755) ;; *) fail 'service path writable by other users';; esac
        service_path=${service_path%/*}
        [ -n "$service_path" ] || service_path=/
    done
    case "$HERE" in "$USB"/*) ;; *) fail 'service bundle must reside on verified USB';; esac
    case "$HERE" in *[!a-zA-Z0-9_./-]*) fail 'unsupported deployment directory characters';; esac
    [ -f /etc/rc.common ] || fail 'OpenWrt rc.common required'
    command -v cmp >/dev/null || fail 'cmp required for exact init template validation'
}
# render_legacy(bundle): render known v1 bytes only; optional bundle defaults to HERE.
render_legacy() {
    legacy_bundle=${1:-$HERE}
    printf '%s\n' '#!/bin/sh /etc/rc.common' 'USE_PROCD=1' 'START=99' 'STOP=10' \
      '# db-relay-updater-procd-v1' \
      'start_service() {' \
      "    /bin/sh '$legacy_bundle/updater-service.sh' check || return 1" \
      '    procd_open_instance' \
      "    procd_set_param command /bin/sh '$legacy_bundle/updater-service.sh' run" \
      '    procd_set_param stdout 0' '    procd_set_param stderr 1' \
      '    procd_set_param term_timeout 240' '    procd_close_instance' '}'
}
# render_v2(bundle): reproduce the complete previously installed v2 template, never execute it.
# Optional bundle defaults to HERE; output is trusted built-in template text.
render_v2() {
    printf '%s\n' '#!/bin/sh /etc/rc.common' 'USE_PROCD=1' 'START=99' 'STOP=10' \
      '# db-relay-updater-procd-v2' "BUNDLE='${1:-$HERE}'" "LOCAL_INIT='$INIT'"
    cat <<'BOOT_GATE'
# trusted_path(path): reject symlinks and non-root/writable ancestors without a USB stat helper.
# Accept only root-owned regular files/directories with conservative modes; returns boolean.
trusted_path() {
    trusted=$1
    while [ "$trusted" != / ]; do
        [ ! -L "$trusted" ] && [ -e "$trusted" ] || return 1
        LC_ALL=C ls -ldn "$trusted" | awk '
          $3 != 0 {exit 1}
          $1 !~ /^[-d]rwx------$/ && $1 !~ /^[-d]rwxr-x---$/ && $1 !~ /^[-d]rwxr-xr-x$/ && $1 !~ /^-rw-------$/ && $1 !~ /^-rw-r--r--$/ {exit 1}
        ' || return 1
        trusted=${trusted%/*}
        [ -n "$trusted" ] || trusted=/
    done
}
# boot_ready(): verify exact rw ext4 UUID and local Docker root; reads only, returns boolean.
boot_ready() {
    trusted_path /mnt/docker-usb || return 1
    mountpoint -q /mnt/docker-usb || return 1
    record=$(awk '$2=="/mnt/docker-usb" {print $1 " " $3 " " $4; count++} END {if(count!=1) exit 1}' /proc/mounts) || return 1
    set -- $record
    [ "$#" = 3 ] && [ "$2" = ext4 ] || return 1
    case ",$3," in *,rw,*) ;; *) return 1;; esac
    device=$(readlink -f "$1") || return 1
    [ -b "$device" ] || return 1
    block info "$device" | awk 'index($0,"UUID=\"d7f45318-dc5b-49db-8250-defb536790c6\"") {ok=1} END {exit !ok}' || return 1
    [ "$(timeout 5 docker --host unix:///var/run/docker.sock info --format '{{.DockerRootDir}}' 2>/dev/null)" = /mnt/docker-usb/docker ] || return 1
    # Protect every loaded script (and binary/helper) against writable or linked bundle entries.
    # Refuse a nested mount anywhere on the bundle ancestry before executing USB code.
    awk -v bundle="$BUNDLE" '$5 != "/" && $5 != "/mnt" && $5 != "/mnt/docker-usb" && (bundle==$5 || index(bundle,$5 "/")==1) {bad=1} END {exit bad}' /proc/self/mountinfo || return 1
    trusted_path "$BUNDLE" || return 1
    for entry in "$BUNDLE"/*.sh "$BUNDLE/relay-updater" "$BUNDLE/deploy-stat"; do
        trusted_path "$entry" || return 1
        awk -v entry="$entry" '$5==entry {bad=1} END {exit bad}' /proc/self/mountinfo || return 1
    done
}
# boot_run(): wait at most 30 bounded probes; procd runs this asynchronously, never rc boot.
boot_run() {
    printf '%s\n' 'relay-updater: waiting for verified USB and local Docker (bounded)' >&2
    attempt=0
    while [ "$attempt" -lt 30 ]; do
        if boot_ready; then
            printf '%s\n' 'relay-updater: USB/Docker ready; starting updater only' >&2
            exec /bin/sh "$BUNDLE/updater-service.sh" run
        fi
        attempt=$((attempt + 1))
        sleep 2
    done
    printf '%s\n' 'relay-updater: readiness timeout; no USB code loaded or containers started; inspect mount/Docker then explicitly start service' >&2
    return 1
}
# start_service(): register local readiness worker only; returns immediately to router startup.
start_service() {
    procd_open_instance
    procd_set_param command /bin/sh "$LOCAL_INIT" boot-run
    procd_set_param stdout 0
    procd_set_param stderr 1
    procd_set_param term_timeout 240
    procd_close_instance
}
if [ "${1:-}" = boot-run ]; then boot_run; exit $?; fi
BOOT_GATE
}
# render_v3(bundle): reproduce the released active-relay gate byte-for-byte for upgrades.
# Optional bundle defaults to HERE; output is the previously trusted template.
render_v3() {
    render_v2 "${1:-$HERE}" | awk '
      $0=="boot_ready() {" {print "storage_ready() {"; next}
      /^if \[ "\$\{1:-\}" = boot-run \];/ {next}
      {gsub("waiting for verified USB and local Docker", "waiting for verified USB, Docker and healthy active relay")}
      {gsub("USB/Docker ready; starting updater only", "USB/Docker/active relay ready; starting updater only")}
      {gsub("inspect mount/Docker then", "inspect mount/Docker/active relay then")}
      {print}
    '
    cat <<'RELAY_GATE'
# boot_ready(): wait for Docker-restored active relay only; never start containers or inspect standby.
# A single bounded snapshot requires managed identity, running state and successful healthcheck.
boot_ready() {
    storage_ready || return 1
    relay_state=$(timeout 5 docker --host unix:///var/run/docker.sock inspect --format '{{.Name}}|{{index .Config.Labels "io.db-relay.deploy.managed"}}|{{.State.Running}}|{{if .State.Health}}{{.State.Health.Status}}{{end}}' db-relay 2>/dev/null) || return 1
    [ "$relay_state" = '/db-relay|relay-v1|true|healthy' ]
}
if [ "${1:-}" = boot-run ]; then boot_run; exit $?; fi
RELAY_GATE
}
# render_service(bundle): embed the bounded Docker probe in local init; no timeout package needed.
# Optional bundle defaults to HERE; emits trusted code before any USB scripts may be loaded.
render_service() {
    render_v3 "${1:-$HERE}" | awk '
      /^if \[ "\$\{1:-\}" = boot-run \];/ {next}
      {gsub("# db-relay-updater-procd-v2", "# db-relay-updater-procd-v4")}
      {gsub("timeout 5 docker", "bounded_docker")}
      {print}
    '
    cat <<'BOUNDED_DOCKER'
# bounded_docker(args...): run one Docker CLI probe for at most five polling intervals.
# Arguments are Docker CLI flags; returns CLI status, 124 on timeout, 143 on interruption.
# Docker is the direct child (no shell wrapper/grandchildren); only this probe is killed.
# A subshell isolates traps and PIDs from procd's worker and leaves stdout unchanged.
bounded_docker() (
    probe_pid=
    trap '
        if [ -n "$probe_pid" ]; then
            kill -KILL "$probe_pid" 2>/dev/null || :
            wait "$probe_pid" 2>/dev/null || :
        fi
    ' EXIT
    trap 'exit 143' HUP INT TERM
    docker "$@" &
    probe_pid=$!
    probe_ticks=0
    while kill -0 "$probe_pid" 2>/dev/null; do
        if [ "$probe_ticks" -ge 5 ]; then
            printf '%s\n' 'relay-updater: Docker readiness probe timed out' >&2
            exit 124
        fi
        sleep 1 || exit 1
        probe_ticks=$((probe_ticks + 1))
    done
    probe_status=0
    wait "$probe_pid" || probe_status=$?
    probe_pid=
    exit "$probe_status"
)
if [ "${1:-}" = boot-run ]; then boot_run; exit $?; fi
BOUNDED_DOCKER
}
# previous_bundle_check(path): require explicit canonical USB directory and trusted root ancestors.
# Does not source/read executable code in the old bundle; returns only after safe_path validation.
previous_bundle_check() {
    case "$1" in "$USB"/*) ;; *) fail 'previous bundle must be strictly beneath verified USB';; esac
    case "$1/" in *[!a-zA-Z0-9_./-]*|*//*|*/../*|*/./*) fail 'previous bundle path must be canonical without dot segments';; esac
    [ "$1" != "${1%/}" ] && fail 'previous bundle must not have trailing slash'
    safe_path "$1"
    [ -d "$1" ] || fail 'previous bundle directory missing'
    previous_path=$1
    while [ "$previous_path" != / ]; do
        [ "$(file_stat -c %u "$previous_path")" = 0 ] || fail 'previous bundle ancestor must be root owned'
        case "$(file_stat -c %a "$previous_path")" in 700|750|755) ;; *) fail 'previous bundle ancestor is writable/untrusted';; esac
        previous_path=${previous_path%/*}
        [ -n "$previous_path" ] || previous_path=/
    done
}
# template_matches(renderer, bundle): compare complete bytes including final newlines, no eval/source.
# Renderer is an internal fixed function name supplied by callers, never init-controlled input.
template_matches() {
    "$1" "$2" | cmp -s "$INIT" -
}
# installed_check(): validate permissions and complete known init contents before invoking it.
installed_check() {
    safe_path "$INIT"
    [ -f "$INIT" ] && [ "$(file_stat -c '%u:%a:%h' "$INIT")" = 0:755:1 ] || fail 'untrusted updater init file'
    template_matches render_service "$HERE" || fail 'init is not current trusted version; explicitly upgrade known template'
}
# install_service(): create absent init or atomically replace exact v1; preserve rc enable links.
install_service() {
    safe_path "$INIT"
    if [ "$MODE" = upgrade ]; then
        [ -f "$INIT" ] && [ "$(file_stat -c '%u:%a:%h' "$INIT")" = 0:755:1 ] || fail 'untrusted updater init file'
        if template_matches render_service "$HERE"; then log 'procd already current; no change'; return 0; fi
        previous_bundle=${PREVIOUS_BUNDLE:-$HERE}
        previous_bundle_check "$previous_bundle"
        template_matches render_legacy "$previous_bundle" || template_matches render_v2 "$previous_bundle" || template_matches render_v3 "$previous_bundle" || template_matches render_service "$previous_bundle" || fail 'unknown or edited init; declare its trusted PREVIOUS_BUNDLE, refusing overwrite'
    else
        [ ! -e "$INIT" ] || fail 'service already installed; use explicit upgrade for known v1'
    fi
    init_temp=$INIT.new
    safe_path "$init_temp"
    (set -C; render_service > "$init_temp")
    chmod 755 "$init_temp"
    sync
    mv "$init_temp" "$INIT"
    sync
    log 'procd installed/upgraded only; enable and start remain explicit operations'
}

service_check
case "$MODE" in
    check) log 'USB/Docker startup prerequisites checked; no services changed';;
    install|upgrade) install_service;;
    enable|start) installed_check; "$INIT" "$MODE";;
    run)
        # Completed retained transactions are eligible; restore validates state and takes the lock.
        # A stale/interrupted journal is not repaired automatically and must not block updater access.
        if [ -e "$USB/relay-updater/lan-owned" ]; then
            if ! sh "$HERE/lan-restore.sh"; then
                log 'LAN restoration deferred: inspect deployment journal/lock; updater will still start'
            fi
        fi
        exec "$HERE/relay-updater" --config "$USB/relay-updater/config.json";;
esac
