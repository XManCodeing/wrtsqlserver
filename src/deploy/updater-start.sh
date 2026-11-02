#!/bin/sh
# Manual host entrypoint; default check is read-only. procd installation is explicit.
# Install relay-updater beside these scripts; config is fixed and never sourced.
# Logs from stdio are discarded (bounded zero bytes); daemon owns safe event logging.
set -eu
HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
. "$HERE/common.sh"
MODE=${1:-check}
[ "$#" -le 1 ] || fail 'usage: updater-start.sh [check|start|install|enable|service-start|run]'
case "$MODE" in
    install|enable|service-start|run)
        [ "$MODE" != service-start ] || MODE=start
        exec sh "$HERE/updater-service.sh" "$MODE";;
    check|start) ;;
    *) fail 'usage: updater-start.sh [check|start|install|enable|service-start|run]';;
esac
UPDATER_HOME=$USB/relay-updater
CONFIG=$UPDATER_HOME/config.json
BINARY=$HERE/relay-updater

# startup_check(): inspect fixed config/executable and immutable startup prerequisites.
# Does not read config contents, create paths or contact the control socket.
startup_check() {
    sh "$HERE/updater-preflight.sh"
    private_file "$CONFIG" 0
    safe_path "$BINARY"
    [ -f "$BINARY" ] && [ -x "$BINARY" ] || fail 'missing packaged relay-updater executable'
    case "$(file_stat -c '%u:%a:%h' "$BINARY")" in 0:700:1|0:750:1|0:755:1) ;; *) fail 'unsafe updater executable';; esac
    for required in nohup sleep; do
        command -v "$required" >/dev/null || fail "missing command: $required"
    done
    safe_path "$UPDATER_HOME/.daemon-lock"
    [ ! -e "$UPDATER_HOME/.daemon-lock" ] || fail 'updater PID lock exists; inspect process manually, never auto-break stale locks'
    [ ! -e "$UPDATER_HOME/run/control.sock" ] || fail 'control socket already exists; investigate running/stale daemon'
}
# start_daemon(): serialize legacy manual starts and detach stdio; procd is preferred.
# Retain lock even after failed startup; crash/stale state requires manual review.
start_daemon() {
    mkdir -m 700 "$UPDATER_HOME/.daemon-lock" || fail 'updater startup lock unavailable'
    nohup "$BINARY" --config "$CONFIG" </dev/null >/dev/null 2>&1 &
    daemon_pid=$!
    printf '%s\n' "$daemon_pid" > "$UPDATER_HOME/.daemon-lock/pid"
    sync
    sleep 1
    kill -0 "$daemon_pid" 2>/dev/null || fail 'updater exited during startup; retained PID lock requires review'
    updater_run_check yes
    log 'updater started manually with detached stdio; private PID lock retained; no system autostart configured'
}

startup_check
[ "$MODE" = start ] || { log 'updater startup check passed; no process started'; exit 0; }
start_daemon
