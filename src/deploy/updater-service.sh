#!/bin/sh
# Explicit procd installer only. Never invoked from the web API or automatically at daemon start.
# install registers but does not enable/start. enable and start are separate explicit operations.
set -eu
HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
. "$HERE/common.sh"
MODE=${1:-check}
case "$MODE" in check|install|enable|start|run) ;; *) fail 'usage: updater-service.sh [check|install|enable|start|run]';; esac
INIT=/etc/init.d/relay-updater

# service_check(): inspect USB/Docker prerequisites and packaged files; never mount/start Docker.
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
}
# install_service(): refuse overwrites and emit a procd runner after storage/Docker startup.
install_service() {
    safe_path "$INIT"
    [ ! -e "$INIT" ] || fail 'service already installed; inspect before replacing'
    (set -C; printf '%s\n' '#!/bin/sh /etc/rc.common' 'USE_PROCD=1' 'START=99' 'STOP=10' \
      '# db-relay-updater-procd-v1' \
      'start_service() {' \
      "    /bin/sh '$HERE/updater-service.sh' check || return 1" \
      '    procd_open_instance' \
      "    procd_set_param command /bin/sh '$HERE/updater-service.sh' run" \
      '    procd_set_param stdout 0' '    procd_set_param stderr 1' \
      '    procd_set_param term_timeout 240' \
      '    procd_close_instance' '}' > "$INIT")
    chmod 755 "$INIT"
    sync
    log 'procd installed only; use explicit enable and start commands after verifying boot mount/Docker order'
}
# installed_check(): only invoke the exact trusted service identity, never an arbitrary init file.
installed_check() {
    safe_path "$INIT"
    [ -f "$INIT" ] && [ "$(file_stat -c '%u:%a:%h' "$INIT")" = 0:755:1 ] || fail 'untrusted updater init file'
    [ "$(awk 'NR==5 {print}' "$INIT")" = '# db-relay-updater-procd-v1' ] || fail 'unknown updater init identity'
}

service_check
case "$MODE" in
    check) log 'USB/Docker startup prerequisites checked; no services changed';;
    install) install_service;;
    enable|start) installed_check; "$INIT" "$MODE";;
    run)
        # fw3 may run before USB/Docker; reconcile once after verified startup, never poll.
        if [ -e "$USB/relay-updater/lan-owned" ] && [ ! -e "$RELAY/transaction" ]; then
            sh "$HERE/lan-restore.sh"
        fi
        # procd owns lifecycle; Go owns singleton lock. No setsid/nohup or SSH dependency.
        exec "$HERE/relay-updater" --config "$USB/relay-updater/config.json";;
esac
