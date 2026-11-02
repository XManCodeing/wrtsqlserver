#!/bin/sh
# Manual only, Docker 20.10 / BusyBox sh. Never interacts with Redis,
# dockerd service, image pull/prune or arbitrary package extraction. Explicit port
# operations reconcile only project-owned mangle PREROUTING LAN RETURN rules.
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
. "$HERE/ports-common.sh"
. "$HERE/lan-rules.sh"
MODE=${1:-check}
[ "$#" -le 1 ] || fail 'one operation argument required'
case "$MODE" in check|apply|rollback|finalize|restart-migrate|boot-check|ports-status|ports-rules|ports-check|ports-apply) ;; *) fail 'unsupported relay operation';; esac
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
    if [ -e "$TX/original-image" ]; then
        private_file "$TX/original-image" 0
        [ "$(docker_local inspect --format '{{.Image}}' "$1")" = "$(cat "$TX/original-image")" ] || fail 'original image identity mismatch'
    fi
}
# check_layout(name): accept only original binds or the exact readonly socket bind.
# Validates bind writability, LAN ports, user and online environment; returns 0.
check_layout() {
    layout_name=${1:-$NAME}
    managed "$layout_name"
    extra_mounts=$(docker_local inspect --format '{{range .Mounts}}{{if ne .Type "bind"}}{{.Type}}:{{.Destination}};{{end}}{{end}}' "$layout_name")
    case "$extra_mounts" in ''|'tmpfs:/tmp;') ;; *) fail 'unsupported non-bind mount';; esac
    # Docker may reorder Mounts across inspect calls. Compare complete records from
    # one snapshot, never pair independent path and RW sequences by array index.
    mount_records=$(docker_local inspect --format '{{range .Mounts}}{{if eq .Type "bind"}}{{printf "%s|%s|%t\n" .Source .Destination .RW}}{{end}}{{end}}' "$layout_name")
    mounts=$(printf '%s\n' "$mount_records" | LC_ALL=C sort)
    base_mounts=$(printf '%s\n' "$RELAY/data|/data|true" "$RELAY/admin_password|/run/secrets/admin_password|false" | LC_ALL=C sort)
    online_mounts=$(printf '%s\n' "$base_mounts" "$USB/relay-updater/run|/run/relay-updater|false" | LC_ALL=C sort)
    online_env=$(docker_local inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$layout_name" | awk '/^ONLINE_UPDATE_SOCKET=/{print}')
    case "$mounts" in
        "$base_mounts")
            [ -z "$online_env" ] || fail 'unsupported original bind environment'
            ;;
        "$online_mounts")
            [ "$online_env" = ONLINE_UPDATE_SOCKET=/run/relay-updater/control.sock ] || fail 'unsupported updater environment'
            if [ "$MODE" = boot-check ]; then updater_run_check no; else updater_run_check yes; fi
            REQUIRE_ONLINE_UPDATE=yes
            ;;
        *) fail 'unsupported bind paths or read/write permissions';;
    esac
    ports=$(docker_local inspect --format '{{range $port, $bindings := .HostConfig.PortBindings}}{{range $bindings}}{{.HostIp}}:{{.HostPort}}:{{$port}};{{end}}{{end}}' "$layout_name")
    ports_layout "$layout_name"
    case "$(docker_local inspect --format '{{.Config.User}} {{.HostConfig.RestartPolicy.Name}}' "$layout_name")" in
        '10001:10001 no'|'10001:10001 unless-stopped') ;;
        *) fail 'unsupported user/restart policy';;
    esac
    security_profile=$(docker_local inspect --format '{{.HostConfig.Privileged}}|{{.HostConfig.ReadonlyRootfs}}|{{.HostConfig.NetworkMode}}|{{.HostConfig.NanoCpus}}|{{.HostConfig.Memory}}|{{.HostConfig.MemorySwap}}|{{.HostConfig.PidsLimit}}|{{json .HostConfig.CapDrop}}|{{json .HostConfig.SecurityOpt}}|{{json .HostConfig.Devices}}|{{.HostConfig.PidMode}}|{{json .HostConfig.CapAdd}}' "$layout_name")
    case "$security_profile" in
        'false|true|default|500000000|134217728|134217728|64|["ALL"]|["no-new-privileges:true"]|[]||null'|'false|true|default|500000000|134217728|134217728|64|["ALL"]|["no-new-privileges:true"]|[]||[]') ;;
        *) fail 'unsupported container security/resource/network layout';;
    esac
}
# full_id(name): print a validated full Docker ID; reject short/ambiguous responses.
full_id() {
    full_value=$(docker_local inspect --format '{{.Id}}' "$1")
    case "$full_value" in ''|*[!0-9a-f]*) fail 'invalid container identity';; esac
    [ "${#full_value}" = 64 ] || fail 'container identity must be full length'
    printf '%s\n' "$full_value"
}
# restart_policy(name, policy): validate layout and pin full identity before policy change.
# Caller holds deployment lock and has validated transaction ownership; returns Docker status.
restart_policy() {
    check_layout "$1"
    restart_id=$(full_id "$1")
    log "setting $1 restart policy to $2"
    docker_local update --restart "$2" "$restart_id" >/dev/null
}
# migrate_restart(): explicitly reconcile completed/current relay roles without starting it.
# Retained completed journals are supported; interrupted journals remain manual recovery.
migrate_restart() {
    check_layout "$NAME"
    if [ -d "$TX" ]; then
        finalize_journal
        if [ -e "$TX/rolled-back" ]; then
            validate_original "$NAME"
            require_absent "$OLD"
            if [ -n "$(container_id "$FAILED")" ]; then
                finalize_candidate "$FAILED"
                standby=$FAILED
            else
                [ ! -e "$TX/candidate-id" ] || fail 'journal candidate unexpectedly missing'
                standby=
            fi
        elif [ -e "$TX/applied" ]; then
            finalize_candidate "$NAME"
            validate_original "$OLD"
            standby=$OLD
            require_absent "$FAILED"
        else
            fail 'unfinished transaction requires explicit rollback before restart migration'
        fi
        if [ -n "$standby" ]; then
            check_layout "$standby"
            [ "$(docker_local inspect --format '{{.State.Running}}' "$standby")" = false ] || fail 'standby container must be stopped'
            restart_policy "$standby" no
        fi
    else
        require_absent "$OLD"
        require_absent "$FAILED"
    fi
    restart_policy "$NAME" unless-stopped
    log 'relay restart migration complete; running/stopped state unchanged'
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
            original-id|candidate-id|original-image|lan-mangle-before) private_file "$journal_entry" 0;;
            ports-original|ports-candidate) ports_read "$journal_entry" >/dev/null;;
            lan-managed) private_file "$journal_entry" 0; [ "$(cat "$journal_entry")" = yes ] || fail 'invalid LAN journal';;
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
    if [ -e "$TX/lan-managed" ]; then lan_rules_check "$(ports_current)"; fi
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
    [ ! -e "$TX/rolled-back" ] || { migrate_restart; log 'transaction already rolled back'; return 0; }
    old_id=$(container_id "$OLD")
    active_id=$(container_id "$NAME")
    if [ -n "$old_id" ]; then
        validate_original "$OLD"
        check_layout "$OLD"
        restart_policy "$OLD" no
        [ "$(docker_local inspect --format '{{.State.Running}}' "$OLD")" = false ] || fail 'rollback container unexpectedly running'
        if [ -n "$active_id" ]; then
            managed "$NAME"
            [ "$(docker_local inspect --format '{{index .Config.Labels "io.db-relay.deploy.transaction"}}' "$NAME")" = "$TRANSACTION" ] || fail 'candidate transaction mismatch'
            save_candidate
            check_layout "$NAME"
            require_absent "$FAILED"
            # Candidate may be unhealthy/crashed; stopping must succeed, but its
            # exit code is not trusted as a migration success signal.
            restart_policy "$NAME" no
            docker_local stop -t 30 "$NAME" >/dev/null
            [ "$(docker_local inspect --format '{{.State.Running}}' "$NAME")" = false ] || fail 'candidate still running'
            docker_local rename "$NAME" "$FAILED"
        fi
        if [ -n "$(container_id "$FAILED")" ]; then
            finalize_candidate "$FAILED"
            restart_policy "$FAILED" no
            [ "$(docker_local inspect --format '{{.State.Running}}' "$FAILED")" = false ] || fail 'failed container unexpectedly running'
        fi
        [ -e "$TX/backup-ready" ] || fail 'old renamed without committed snapshot'
        restore_pair
        docker_local rename "$OLD" "$NAME"
    else
        [ -n "$active_id" ] || fail 'original container unavailable'
        validate_original "$NAME"
        check_layout "$NAME"
        # No candidate can have run until original was renamed. If restore began
        # and rename already completed, validate restored files before restart.
        if [ -e "$TX/restore-intent" ]; then
            [ -e "$TX/restored" ] || fail 'ambiguous restore state'
            validate_data "$RELAY/data"
        fi
    fi
    ports_restore
    if [ -n "$(container_id "$FAILED")" ]; then
        finalize_candidate "$FAILED"
        restart_policy "$FAILED" no
        [ "$(docker_local inspect --format '{{.State.Running}}' "$FAILED")" = false ] || fail 'failed container unexpectedly running'
    fi
    validate_original "$NAME"
    restart_policy "$NAME" no
    log 'restarting original relay only'
    docker_local start "$restart_id" >/dev/null
    relay_health || fail 'original relay health failed; retained journal requires review'
    mark rolled-back
    restart_policy "$NAME" unless-stopped
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
    [ ! -e "$TX" ] || fail 'unexpected transaction path'
    new_dir "$TX" 0
    docker_local inspect --format '{{.Id}}' "$NAME" > "$TX/original-id"
    docker_local inspect --format '{{.Image}}' "$NAME" > "$TX/original-image"
    sync
    ORIGINAL=$(original_id)
    if [ "$MODE" = ports-apply ]; then
        [ "$(cat "$TX/original-image")" = "$IMAGE_ID" ] || fail 'same-image port rebuild identity changed'
    fi
    TRANSACTION=$ORIGINAL
    # Snapshot policies for upgrades too, so future image updates preserve dynamic ports.
    if [ -d "$PORT_HOME" ]; then ports_snapshot; fi
    RECOVER_ON_EXIT=yes
    mark stopping
    log 'stopping relay for consistent config/key backup; Redis unaffected'
    validate_original "$NAME"
    restart_policy "$NAME" no
    stop_checked "$NAME"
    backup_pair
    validate_original "$NAME"
    docker_local rename "$NAME" "$OLD"
    mark launching
    RELAY_RESTART_POLICY=no relay_run "$IMAGE_ID"
    save_candidate
    if [ -e "$TX/ports-original" ]; then ports_commit; fi
    relay_health || fail 'candidate health failed'
    if [ "$MODE" = ports-apply ]; then
        safe_path "$PORT_HOME/ports-migrated"
        if [ -e "$PORT_HOME/ports-migrated" ]; then
            private_file "$PORT_HOME/ports-migrated" 0
        else
            (set -C; printf 'yes\n' > "$PORT_HOME/ports-migrated")
        fi
        sync
    fi
    mark applied
    finalize_candidate "$NAME"
    restart_policy "$NAME" unless-stopped
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
if [ "$MODE" = ports-status ] || [ "$MODE" = ports-rules ]; then
    check_layout
    if [ "$MODE" = ports-rules ]; then
        private_file "$PORT_HOME/lan-owned" 0
        if [ -d "$PORT_HOME/lan-migration" ]; then
            private_file "$PORT_HOME/lan-migration/state" 0
            [ "$(cat "$PORT_HOME/lan-migration/state")" = complete ] || fail 'LAN migration manual_recovery'
        fi
        lan_rules_check "${RELAY_PORTS-}"
    fi
    exit 0
fi
if [ "$MODE" = ports-check ] && [ -d "$TX" ]; then
    [ -e "$TX/applied" ] || [ -e "$TX/rolled-back" ] || fail 'unfinished transaction blocks port preflight'
    finalize_journal
    check_layout
    ports_preflight
    exit 0
fi
if [ "$MODE" = boot-check ]; then
    check_layout "$NAME"
    if [ -d "$TX" ]; then
        finalize_journal
        if [ -e "$TX/rolled-back" ]; then
            validate_original "$NAME"
            require_absent "$OLD"
            if [ -n "$(container_id "$FAILED")" ]; then
                finalize_candidate "$FAILED"
            else
                [ ! -e "$TX/candidate-id" ] || fail 'journal candidate unexpectedly missing'
            fi
        elif [ -e "$TX/applied" ]; then
            finalize_candidate "$NAME"
            validate_original "$OLD"
            check_layout "$OLD"
            require_absent "$FAILED"
        else
            fail 'unfinished transaction blocks LAN restore'
        fi
    else
        require_absent "$OLD"
        require_absent "$FAILED"
    fi
    exit 0
fi
if [ "$MODE" = restart-migrate ]; then
    acquire_lock "$RELAY"
    trap finish EXIT
    trap 'exit 130' INT
    trap 'exit 143' HUP TERM
    migrate_restart
    exit 0
fi
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
if [ "$MODE" = ports-check ] || [ "$MODE" = ports-apply ]; then
    ports_preflight
    IMAGE_ID=$(docker_local inspect --format '{{.Image}}' "$NAME")
    case "$IMAGE_ID" in sha256:*) ;; *) fail 'invalid current image identity';; esac
else
    RELAY_PORTS=$(ports_current)
    image_check "${RELAY_IMAGE:-}"
    [ "$IMAGE_ID" != "$(docker_local inspect --format '{{.Image}}' "$NAME")" ] || fail 'candidate image equals current image'
fi
log 'update preflight passed; check made no writes; apply temporarily stops relay'
[ "$MODE" = apply ] || [ "$MODE" = ports-apply ] || exit 0
acquire_lock "$RELAY"
trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' HUP TERM
# Repeat critical state checks after locking; shell guards do not protect against
# hostile root or concurrent external Docker administrators/unmounts.
[ ! -e "$TX" ] || fail 'transaction appeared while locking'
check_layout
if [ "$MODE" = ports-apply ]; then ports_preflight; fi
require_absent "$OLD"
require_absent "$FAILED"
apply_update
