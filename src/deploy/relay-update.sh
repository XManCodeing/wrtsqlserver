#!/bin/sh
# Manual only, Docker 20.10 / BusyBox sh. Never interacts with Redis, firewall,
# dockerd service, images pull/prune, archives or arbitrary package extraction.
# LAN_INTERFACE=br-lan LAN_BIND_IP=<LAN IPv4> LAN_INTERFACE_CONFIRMED=yes \
# RELAY_IMAGE=local/db-relay:1.0.1 sh relay-update.sh [check|apply|rollback|finalize]
# check is read-only. apply accepts only locally imported linux/amd64 images.
# One retained transaction blocks another update until explicit finalize archives it.
# finalize retains all snapshots and removes only verified stopped relay containers.
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
[ "$#" -le 1 ] || fail 'usage: relay-update.sh [check|apply|rollback|finalize]'
case "$MODE" in check|apply|rollback|finalize) ;; *) fail 'usage: relay-update.sh [check|apply|rollback|finalize]';; esac
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
# check_layout(name): accept only original binds or the exact readonly socket bind.
# Validates bind writability, LAN ports, user and online environment; returns 0.
check_layout() {
    layout_name=${1:-$NAME}
    managed "$layout_name"
    extra_mounts=$(docker_local inspect --format '{{range .Mounts}}{{if ne .Type "bind"}}{{.Type}}:{{.Destination}};{{end}}{{end}}' "$layout_name")
    case "$extra_mounts" in ''|'tmpfs:/tmp;') ;; *) fail 'unsupported non-bind mount';; esac
    mounts=$(docker_local inspect --format '{{range .Mounts}}{{if eq .Type "bind"}}{{.Source}}:{{.Destination}};{{end}}{{end}}' "$layout_name")
    base_mounts="$RELAY/data:/data;$RELAY/admin_password:/run/secrets/admin_password;"
    online_env=$(docker_local inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$layout_name" | awk '/^ONLINE_UPDATE_SOCKET=/{print}')
    bind_modes=$(docker_local inspect --format '{{range .Mounts}}{{if eq .Type "bind"}}{{.RW}};{{end}}{{end}}' "$layout_name")
    case "$mounts" in
        "$base_mounts")
            [ "$bind_modes" = 'true;false;' ] && [ -z "$online_env" ] || fail 'unsupported original bind modes/environment'
            ;;
        "$base_mounts$USB/relay-updater/run:/run/relay-updater;"|"$RELAY/data:/data;$USB/relay-updater/run:/run/relay-updater;$RELAY/admin_password:/run/secrets/admin_password;")
            [ "$bind_modes" = 'true;false;false;' ] || fail 'updater bind must be readonly'
            [ "$online_env" = ONLINE_UPDATE_SOCKET=/run/relay-updater/control.sock ] || fail 'unsupported updater environment'
            updater_run_check yes
            REQUIRE_ONLINE_UPDATE=yes
            ;;
        *) fail 'unsupported bind layout';;
    esac
    ports=$(docker_local inspect --format '{{range $port, $bindings := .HostConfig.PortBindings}}{{range $bindings}}{{.HostIp}}:{{.HostPort}}:{{$port}};{{end}}{{end}}' "$layout_name")
    [ "$ports" = "$LAN_BIND_IP:13306:13306/tcp;$LAN_BIND_IP:18080:8080/tcp;" ] || fail 'LAN/ports differ from managed deployment'
    [ "$(docker_local inspect --format '{{.Config.User}} {{.HostConfig.RestartPolicy.Name}}' "$layout_name")" = '10001:10001 no' ] || fail 'unsupported user/restart policy'
}
# full_id(name): print a validated full Docker ID; reject short/ambiguous responses.
full_id() {
    full_value=$(docker_local inspect --format '{{.Id}}' "$1")
    case "$full_value" in ''|*[!0-9a-f]*) fail 'invalid container identity';; esac
    [ "${#full_value}" = 64 ] || fail 'container identity must be full length'
    printf '%s\n' "$full_value"
}
# save_candidate(): persist exact candidate identity before accepting its health.
# Recovery may call this after docker run returned an ambiguous failure.
save_candidate() {
    candidate_value=$(full_id "$NAME")
    [ "$candidate_value" != "$ORIGINAL" ] || fail 'candidate equals original identity'
    if [ -e "$TX/candidate-id" ]; then
        private_file "$TX/candidate-id" 0
        [ "$(cat "$TX/candidate-id")" = "$candidate_value" ] || fail 'candidate journal identity mismatch'
    else
        (set -C; printf '%s\n' "$candidate_value" > "$TX/candidate-id")
        sync
    fi
}
# finalize_journal(): reject unknown, malformed or interrupted finalization state.
# Retains every data/secret backup; returns only for completed transaction markers.
finalize_journal() {
    ORIGINAL=$(original_id)
    [ ! -e "$TX/finalizing" ] || fail 'interrupted finalization requires manual review'
    for journal_entry in "$TX"/* "$TX"/.[!.]* "$TX"/..?*; do
        [ -e "$journal_entry" ] || [ -L "$journal_entry" ] || continue
        case "${journal_entry##*/}" in
            original-id|candidate-id) private_file "$journal_entry" 0;;
            backup)
                private_dir "$journal_entry" 0
                private_file "$journal_entry/config.enc" 0
                private_file "$journal_entry/secret.key" 0;;
            failed-data) validate_data "$journal_entry";;
            stopping|backup-ready|launching|applied|restore-intent|restored|rolled-back)
                private_file "$journal_entry" 0
                [ "$(cat "$journal_entry")" = yes ] || fail 'invalid transaction marker';;
            *) fail 'unknown transaction state';;
        esac
    done
    [ -e "$TX/stopping" ] || fail 'missing stopping marker'
    if [ -e "$TX/applied" ]; then
        [ -e "$TX/launching" ] && [ -e "$TX/backup-ready" ] && [ -e "$TX/candidate-id" ] || fail 'incomplete applied transaction'
    fi
    if [ -e "$TX/restore-intent" ]; then
        [ -e "$TX/restored" ] && [ -e "$TX/rolled-back" ] && [ -d "$TX/failed-data" ] || fail 'incomplete restore transaction'
    fi
    if [ -e "$TX/launching" ]; then
        [ -e "$TX/backup-ready" ] || fail 'launching without committed backup'
    fi
    if [ -e "$TX/candidate-id" ]; then
        [ -e "$TX/launching" ] || fail 'candidate without launching marker'
        if [ -e "$TX/rolled-back" ]; then
            [ -e "$TX/restore-intent" ] && [ -e "$TX/restored" ] || fail 'candidate rollback without completed restore'
        fi
    fi
    if [ -e "$TX/restored" ] || [ -d "$TX/failed-data" ]; then
        [ -e "$TX/restore-intent" ] || fail 'restore artifacts without intent'
    fi
    if [ -e "$TX/backup-ready" ]; then
        private_dir "$TX/backup" 0
        private_file "$TX/backup/config.enc" 0
        private_file "$TX/backup/secret.key" 0
    fi
}
# finalize_candidate(name): verify exact journal identity, transaction and layout.
finalize_candidate() {
    private_file "$TX/candidate-id" 0
    [ "$(full_id "$1")" = "$(cat "$TX/candidate-id")" ] || fail 'candidate identity mismatch'
    [ "$(full_id "$1")" != "$ORIGINAL" ] || fail 'candidate equals original'
    [ "$(docker_local inspect --format '{{index .Config.Labels "io.db-relay.deploy.transaction"}}' "$1")" = "$ORIGINAL" ] || fail 'candidate transaction mismatch'
    check_layout "$1"
}
# finalize_update(): archive completed journal and remove only verified stopped IDs.
# Any interruption after finalizing intent fails closed; no automatic deletion retry.
finalize_update() {
    finalize_journal
    cleanup_name=
    cleanup_id=
    check_layout "$NAME"
    if [ -e "$TX/rolled-back" ]; then
        validate_original "$NAME"
        require_absent "$OLD"
        if [ -n "$(container_id "$FAILED")" ]; then
            finalize_candidate "$FAILED"
            cleanup_name=$FAILED
        else
            [ ! -e "$TX/candidate-id" ] || fail 'journal candidate unexpectedly missing'
        fi
    elif [ -e "$TX/applied" ]; then
        finalize_candidate "$NAME"
        validate_original "$OLD"
        check_layout "$OLD"
        require_absent "$FAILED"
        cleanup_name=$OLD
    else
        fail 'only completed applied or rolled-back transactions can finalize'
    fi
    validate_data "$RELAY/data"
    relay_health || fail 'active relay must be healthy before finalize'
    if [ -n "$cleanup_name" ]; then
        cleanup_id=$(full_id "$cleanup_name")
        [ "$(docker_local inspect --format '{{.State.Status}}' "$cleanup_id")" = exited ] || fail 'cleanup container must be exited'
        [ "$(docker_local inspect --format '{{.State.Running}}' "$cleanup_id")" = false ] || fail 'refusing to remove running container'
    fi
    safe_path "$RELAY/history"
    [ -e "$RELAY/history" ] || new_dir "$RELAY/history" 0
    private_dir "$RELAY/history" 0
    archive_target="$RELAY/history/transaction-$ORIGINAL-$(date +%s)-$$"
    safe_path "$archive_target"
    [ ! -e "$archive_target" ] || fail 'history destination already exists'
    mark finalizing
    if [ -n "$cleanup_id" ]; then
        [ "$(full_id "$cleanup_name")" = "$cleanup_id" ] || fail 'cleanup identity changed'
        [ "$(docker_local inspect --format '{{.State.Running}}' "$cleanup_id")" = false ] || fail 'cleanup container started'
        docker_local rm "$cleanup_id" >/dev/null
    fi
    mv "$TX" "$archive_target"
    sync
    log 'finalized: journal and all backups retained in private history'
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
    [ ! -e "$TX/finalizing" ] || fail 'interrupted finalization requires manual review'
    TRANSACTION=$ORIGINAL
    [ ! -e "$TX/rolled-back" ] || { log 'transaction already rolled back'; return 0; }
    old_id=$(container_id "$OLD")
    active_id=$(container_id "$NAME")
    if [ -n "$old_id" ]; then
        validate_original "$OLD"
        if [ -n "$active_id" ]; then
            managed "$NAME"
            [ "$(docker_local inspect --format '{{index .Config.Labels "io.db-relay.deploy.transaction"}}' "$NAME")" = "$TRANSACTION" ] || fail 'candidate transaction mismatch'
            save_candidate
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
    save_candidate
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
if [ "$MODE" = rollback ] || [ "$MODE" = finalize ]; then
    private_dir "$TX" 0
    acquire_lock "$RELAY"
    trap finish EXIT
    trap 'exit 130' INT
    trap 'exit 143' HUP TERM
    if [ "$MODE" = finalize ]; then finalize_update; else recover; fi
    exit 0
fi
[ ! -e "$TX" ] || fail 'retained/interrupted transaction: explicit rollback or finalize required'
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
