#!/bin/sh
# Manual only, Docker 20.10 / BusyBox sh. Never interacts with Redis, firewall,
# dockerd service, images pull/prune, archives or arbitrary package extraction.
# LAN_INTERFACE=br-lan LAN_BIND_IP=<LAN IPv4> LAN_INTERFACE_CONFIRMED=yes \
# RELAY_IMAGE=local/db-relay:1.0.1 sh relay-update.sh [check|apply|rollback]
# check is read-only. apply accepts only locally imported linux/amd64 images.
# This conservative version supports ONE retained update transaction at a time.
# Existing rollback/failed containers or journal block another apply: archive and
# review them manually, never automatically delete historical data/containers.
# rollback restores the STOPPED-service snapshot, discarding no files: the new
# version's data is retained under transaction/failed-data. Post-upgrade edits
# will no longer be active. Do not use with external writers/other deploy tools.
# SIGKILL/power failure leaves a journal and possibly stale lock. After checking
# no deployment is active, run rollback with STALE_LOCK_CONFIRMED=yes. Recovery
# never guesses ambiguous identity/layout; such states require manual review.
set -eu
HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
. "$HERE/common.sh"
MODE=${1:-check}
[ "$#" -le 1 ] || fail 'usage: relay-update.sh [check|apply|rollback]'
case "$MODE" in check|apply|rollback) ;; *) fail 'usage: relay-update.sh [check|apply|rollback]';; esac
TX=$RELAY/transaction
FAILED=db-relay-failed
TRANSACTION=upgrade-v1
RECOVER_ON_EXIT=no

# validate_data(path): permit only the two private regular configuration files.
# Reject unknown files rather than silently lose unrecognized application state.
validate_data() {
    private_dir "$1" 10001
    private_file "$1/config.enc" 10001
    private_file "$1/secret.key" 10001
    for data_entry in "$1"/* "$1"/.[!.]* "$1"/..?*; do
        [ -e "$data_entry" ] || [ -L "$data_entry" ] || continue
        case "${data_entry##*/}" in config.enc|secret.key) ;; *) fail "unrecognized data file: $data_entry";; esac
    done
}
# mark(name): atomically create a root-only durable phase marker; returns IO status.
mark() {
    safe_path "$TX/$1"
    [ ! -e "$TX/$1" ] || return 0
    (set -C; printf 'yes\n' > "$TX/$1")
    sync
}
# original_id(): validate journal and emit original full container ID.
original_id() {
    private_dir "$TX" 0
    private_file "$TX/original-id" 0
    saved_id=$(cat "$TX/original-id")
    case "$saved_id" in ''|*[!0-9a-f]*) fail 'invalid journal container identity';; esac
    [ "${#saved_id}" = 64 ] || fail 'invalid journal ID length'
    printf '%s\n' "$saved_id"
}
# validate_original(name): verify managed label, exact saved ID and stable binds.
validate_original() {
    managed "$1"
    [ "$(docker_local inspect --format '{{.Id}}' "$1")" = "$ORIGINAL" ] || fail 'original container identity mismatch'
}
# check_layout(): allow only script-created bind mounts and original LAN ports.
check_layout() {
    managed "$NAME"
    mounts=$(docker_local inspect --format '{{range .Mounts}}{{if eq .Type "bind"}}{{.Source}}:{{.Destination}};{{end}}{{end}}' "$NAME")
    [ "$mounts" = "$RELAY/data:/data;$RELAY/admin_password:/run/secrets/admin_password;" ] || fail 'unsupported bind layout'
    ports=$(docker_local inspect --format '{{range $port, $bindings := .HostConfig.PortBindings}}{{range $bindings}}{{.HostIp}}:{{.HostPort}}:{{$port}};{{end}}{{end}}' "$NAME")
    [ "$ports" = "$LAN_BIND_IP:13306:13306/tcp;$LAN_BIND_IP:18080:8080/tcp;" ] || fail 'LAN/ports differ from managed deployment'
    [ "$(docker_local inspect --format '{{.Config.User}} {{.HostConfig.RestartPolicy.Name}}' "$NAME")" = '10001:10001 no' ] || fail 'unsupported user/restart policy'
}
# backup_pair(): called ONLY after verified stopped original; commit marker last.
backup_pair() {
    validate_data "$RELAY/data"
    new_dir "$TX/backup" 0
    cp "$RELAY/data/config.enc" "$TX/backup/config.enc"
    cp "$RELAY/data/secret.key" "$TX/backup/secret.key"
    chmod 600 "$TX/backup/config.enc" "$TX/backup/secret.key"
    private_file "$TX/backup/config.enc" 0
    private_file "$TX/backup/secret.key" 0
    sync
    mark backup-ready
}
# stop_checked(name): stop only managed relay; reject timeout-forced exit 137.
stop_checked() {
    managed "$1"
    docker_local stop -t 30 "$1" >/dev/null
    [ "$(docker_local inspect --format '{{.State.Running}} {{.State.ExitCode}}' "$1")" = 'false 0' ] || fail 'relay did not exit cleanly; recovery requires manual review'
}
# restore_pair(): retain new data and atomically install a private snapshot copy.
# Intent marker makes rename interruptions recoverable without overwriting data.
restore_pair() {
    private_dir "$TX/backup" 0
    private_file "$TX/backup/config.enc" 0
    private_file "$TX/backup/secret.key" 0
    safe_path "$TX/failed-data"
    safe_path "$RELAY/data"
    if [ ! -e "$TX/restore-intent" ]; then
        [ ! -e "$TX/failed-data" ] || fail 'unexpected retained data'
        mark restore-intent
    fi
    if [ ! -e "$TX/failed-data" ]; then
        private_dir "$RELAY/data" 10001
        mv "$RELAY/data" "$TX/failed-data"
        sync
    fi
    if [ ! -e "$RELAY/data" ]; then
        # An interrupted preparation is refused rather than overwriting files.
        new_dir "$TX/restore-data" 10001
        cp "$TX/backup/config.enc" "$TX/restore-data/config.enc"
        cp "$TX/backup/secret.key" "$TX/restore-data/secret.key"
        chown 10001:10001 "$TX/restore-data/config.enc" "$TX/restore-data/secret.key"
        chmod 600 "$TX/restore-data/config.enc" "$TX/restore-data/secret.key"
        sync
        mv "$TX/restore-data" "$RELAY/data"
        sync
    fi
    validate_data "$RELAY/data"
    cmp -s "$RELAY/data/config.enc" "$TX/backup/config.enc" || fail 'restored configuration mismatch'
    cmp -s "$RELAY/data/secret.key" "$TX/backup/secret.key" || fail 'restored key mismatch'
    mark restored
}
# recover(): idempotent conservative rollback; only named+labelled journal IDs.
recover() {
    ORIGINAL=$(original_id)
    TRANSACTION=$ORIGINAL
    [ ! -e "$TX/rolled-back" ] || { log 'transaction already rolled back'; return 0; }
    old_id=$(container_id "$OLD")
    active_id=$(container_id "$NAME")
    if [ -n "$old_id" ]; then
        validate_original "$OLD"
        if [ -n "$active_id" ]; then
            managed "$NAME"
            [ "$(docker_local inspect --format '{{index .Config.Labels "io.db-relay.deploy.transaction"}}' "$NAME")" = "$TRANSACTION" ] || fail 'candidate transaction mismatch'
            require_absent "$FAILED"
            # Candidate may be unhealthy/crashed; stopping must succeed, but its
            # exit code is not trusted as a migration success signal.
            docker_local stop -t 30 "$NAME" >/dev/null
            [ "$(docker_local inspect --format '{{.State.Running}}' "$NAME")" = false ] || fail 'candidate still running'
            docker_local rename "$NAME" "$FAILED"
        fi
        [ -e "$TX/backup-ready" ] || fail 'old renamed without committed snapshot'
        restore_pair
        docker_local rename "$OLD" "$NAME"
    else
        [ -n "$active_id" ] || fail 'original container unavailable'
        validate_original "$NAME"
        # No candidate can have run until original was renamed. If restore began
        # and rename already completed, validate restored files before restart.
        if [ -e "$TX/restore-intent" ]; then
            [ -e "$TX/restored" ] || fail 'ambiguous restore state'
            validate_data "$RELAY/data"
        fi
    fi
    log 'restarting original relay only'
    docker_local start "$NAME" >/dev/null
    relay_health || fail 'original relay health failed; retained journal requires review'
    mark rolled-back
    log 'rollback complete; prior config/key restored; all candidate data retained'
}
# finish(status): trap handler; run rollback in isolated subshell to catch failures.
finish() {
    finish_status=$?
    trap - EXIT HUP INT TERM
    if [ "$RECOVER_ON_EXIT" = yes ]; then
        log 'update interrupted/failed; attempting relay-only rollback'
        # Do not call recover in an if/! condition: POSIX sh would disable -e
        # throughout nested functions, potentially continuing after failed IO.
        set +e
        (set -e; recover)
        recovery_status=$?
        set -e
        if [ "$recovery_status" -ne 0 ]; then
            log 'AUTOMATIC ROLLBACK FAILED: retain journal; manually investigate before rollback'
        fi
    fi
    release_lock || log 'lock cleanup failed; manual review required'
    exit "$finish_status"
}
# apply_update(): journal before any stop; snapshot while stopped; retain original.
apply_update() {
    new_dir "$TX" 0
    docker_local inspect --format '{{.Id}}' "$NAME" > "$TX/original-id"
    sync
    ORIGINAL=$(original_id)
    TRANSACTION=$ORIGINAL
    RECOVER_ON_EXIT=yes
    mark stopping
    log 'stopping relay for consistent config/key backup; Redis unaffected'
    stop_checked "$NAME"
    backup_pair
    validate_original "$NAME"
    docker_local rename "$NAME" "$OLD"
    mark launching
    relay_run "$IMAGE_ID"
    relay_health || fail 'candidate health failed'
    mark applied
    RECOVER_ON_EXIT=no
    log 'update healthy; stopped rollback container and consistent snapshot retained'
}

usb_check
lan_check
command -v wget >/dev/null || fail 'wget required'
command -v cmp >/dev/null || fail 'cmp required'
private_dir "$RELAY" 0
private_file "$RELAY/admin_password" 10001
safe_path "$TX"
if [ "$MODE" = rollback ]; then
    private_dir "$TX" 0
    acquire_lock "$RELAY"
    trap finish EXIT
    trap 'exit 130' INT
    trap 'exit 143' HUP TERM
    recover
    exit 0
fi
[ ! -e "$TX" ] || fail 'retained/interrupted transaction: only explicit rollback or manual archival permitted'
[ ! -e "$RELAY/.deploy-lock" ] || fail 'deployment lock exists; investigate before changes'
check_layout
[ "$(docker_local inspect --format '{{.State.Running}}' "$NAME")" = true ] || fail 'relay must be running before update'
require_absent "$OLD"
require_absent "$FAILED"
validate_data "$RELAY/data"
image_check "${RELAY_IMAGE:-}"
[ "$IMAGE_ID" != "$(docker_local inspect --format '{{.Image}}' "$NAME")" ] || fail 'candidate image equals current image'
log 'update preflight passed; check made no writes; apply temporarily stops relay'
[ "$MODE" = apply ] || exit 0
acquire_lock "$RELAY"
trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' HUP TERM
# Repeat critical state checks after locking; shell guards do not protect against
# hostile root or concurrent external Docker administrators/unmounts.
[ ! -e "$TX" ] || fail 'transaction appeared while locking'
check_layout
require_absent "$OLD"
require_absent "$FAILED"
apply_update
