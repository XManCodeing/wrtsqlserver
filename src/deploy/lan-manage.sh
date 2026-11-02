#!/bin/sh
# Explicit migration of the confirmed relay_lan_redis include; no automatic router changes.
set -eu
HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
. "$HERE/common.sh"
. "$HERE/ports-common.sh"
. "$HERE/lan-rules.sh"
MODE=${1:-check}
case "$MODE" in check|install|restore|verify) ;; *) fail 'usage: lan-manage.sh [check|install|restore|verify]';; esac
LEGACY=$PORT_HOME/lan-redis-bypass.sh
BACKUP=$PORT_HOME/lan-migration

# legacy_rule(action, kind): address the exact confirmed mangle rule, never Redis mutations.
legacy_rule() {
    legacy_action=$1; legacy_kind=$2
    set -- -i "$LAN_INTERFACE" -s "$LAN_CIDR" -d "$LAN_BIND_IP/32" -p tcp
    if [ "$legacy_kind" = redis ]; then
        set -- "$@" --dport 6379 -m comment --comment relay-lan-redis-bypass -j RETURN
    else
        set -- "$@" -m multiport --dports 18080,13306 -m comment --comment relay-lan-app-bypass -j RETURN
    fi
    iptables -w 5 -t mangle "$legacy_action" PREROUTING "$@"
}
# legacy_validate(extract): parse a restricted grammar, optionally emit unchanged Redis prefix.
legacy_validate() {
    safe_path "$HERE/legacy-lan-validate.awk"
    case "$(file_stat -c '%u:%a:%h' "$HERE/legacy-lan-validate.awk")" in 0:600:1|0:644:1|0:700:1|0:755:1) ;; *) fail 'untrusted legacy parser';; esac
    [ "$(wc -c < "$LEGACY" | tr -d ' ')" -le 16384 ] || fail 'oversized legacy script'
    awk -v iface="$LAN_INTERFACE" -v source="$LAN_CIDR" -v destination="$LAN_BIND_IP/32" \
      -v extract="$1" -f "$HERE/legacy-lan-validate.awk" "$LEGACY" || fail 'unknown legacy script grammar; no code was executed'
}
# migration_check(): require clean UCI, exact old include and absent destination entries.
migration_check() {
    safe_path "$LEGACY"
    [ -f "$LEGACY" ] && [ "$(file_stat -c '%u:%a:%h' "$LEGACY")" = 0:700:1 ] || fail 'trusted root 0700 legacy script required'
    lan_scope
    legacy_validate 0
    pending_uci=$(uci changes firewall) || fail 'cannot inspect pending firewall changes'
    [ -z "$pending_uci" ] || fail 'uncommitted firewall UCI changes; refusing takeover'
    [ "$(uci -q get firewall.relay_lan_redis)" = include ] || fail 'legacy include identity mismatch'
    [ "$(uci -q get firewall.relay_lan_redis.type)" = script ] || fail 'legacy include type mismatch'
    [ "$(uci -q get firewall.relay_lan_redis.path)" = "$LEGACY" ] || fail 'legacy include path mismatch'
    [ "$(uci -q get firewall.relay_lan_redis.reload)" = 1 ] || fail 'legacy reload mismatch'
    [ "$(uci -q get firewall.relay_lan_redis.enabled)" = 1 ] || fail 'legacy enabled mismatch'
    if uci -q get firewall.db_relay_ports >/dev/null; then fail 'new UCI entry already exists'; fi
    firewall_entries=$(uci show firewall) || fail 'cannot inspect firewall UCI'
    include_entries=$(printf '%s\n' "$firewall_entries" | awk -v path="$LEGACY" 'index($0,path){count++} END{print count+0}')
    [ "$include_entries" = 1 ] || fail 'ambiguous legacy loading entries'
    for migration_path in "$BACKUP" "$PORT_HOME/lan-owned" "$PORT_HOME/lan-scope"; do
        safe_path "$migration_path"
        [ ! -e "$migration_path" ] || fail 'migration artifacts exist; manual recovery required'
    done
    [ ! -e "$RELAY/transaction" ] || fail 'retained transaction blocks LAN takeover'
    legacy_rule -C redis || fail 'confirmed Redis rule missing'
    legacy_rule -C app || fail 'confirmed application multiport rule missing'
    rules_before=$(iptables -w 5 -t mangle -S PREROUTING)
    printf '%s\n' "$rules_before" | awk 'index($0,"relay-lan-redis-bypass"){redis++} index($0,"relay-lan-app-bypass"){app++} index($0,"db-relay-lan-v1"){owned++} END{exit(redis!=1 || app!=1 || owned!=0)}' || fail 'duplicate or unknown migration rules'
}
# migration_finish(): retain durable evidence on every failure, never guess rollback/UCI state.
migration_finish() {
    migration_status=$?
    trap - EXIT HUP INT TERM
    if [ "$migration_status" -ne 0 ] && [ -d "$BACKUP" ]; then
        printf 'manual_recovery\n' > "$BACKUP/state"
        sync
        log 'manual recovery required: original script/UCI/mangle snapshots retained; no automatic replay'
    fi
    release_lock || log 'migration lock cleanup failed; inspect manually'
    exit "$migration_status"
}
# migration_install(): save all evidence before replacing only the application portion.
migration_install() {
    acquire_lock "$RELAY"
    trap migration_finish EXIT
    trap 'exit 130' INT
    trap 'exit 143' HUP TERM
    sh "$HERE/relay-update.sh" ports-status
    ports_available '3306,13306'
    migration_check
    new_dir "$BACKUP" 0
    cp "$LEGACY" "$BACKUP/legacy-script"
    uci export firewall > "$BACKUP/firewall-uci"
    printf '%s\n' "$rules_before" > "$BACKUP/mangle-before"
    printf 'preparing\n' > "$BACKUP/state"
    legacy_validate 1 > "$BACKUP/redis-only"
    chmod 600 "$BACKUP"/*
    sh -n "$BACKUP/redis-only"
    sync
    write_lan_scope
    lan_rules_apply "$(ports_current)"
    legacy_temporary=$PORT_HOME/.lan-redis-$$
    (set -C; cat "$BACKUP/redis-only" > "$legacy_temporary")
    chmod 700 "$legacy_temporary"
    mv "$legacy_temporary" "$LEGACY"
    sync
    # Never delete/reinsert Redis; only the exact old application multiport rule is removed.
    legacy_rule -D app
    legacy_rule -C redis
    pending_uci=$(uci changes firewall) || fail 'cannot inspect pending firewall changes'
    [ -z "$pending_uci" ] || fail 'UCI changed during migration; manual recovery required'
    if uci -q get firewall.db_relay_ports >/dev/null; then fail 'new include appeared during migration'; fi
    uci set firewall.db_relay_ports=include
    uci set firewall.db_relay_ports.type=script
    uci set firewall.db_relay_ports.path="$HERE/lan-restore.sh"
    uci set firewall.db_relay_ports.reload=1
    uci set firewall.db_relay_ports.enabled=1
    uci commit firewall
    lan_rules_check "$(ports_current)"
    printf 'lan-v1\n' > "$PORT_HOME/lan-owned"
    printf 'complete\n' > "$BACKUP/state"
    sync
    log 'LAN takeover complete; unchanged Redis prefix/include retained; 3306 requires explicit port apply'
}
# write_lan_scope(): store inert key/value data, not executable shell content.
write_lan_scope() {
    (set -C; printf 'LAN_INTERFACE=%s\nLAN_BIND_IP=%s\n' "$LAN_INTERFACE" "$LAN_BIND_IP" > "$PORT_HOME/lan-scope")
}

usb_check
lan_check
private_dir "$PORT_HOME" 0
case "$MODE" in
    check) sh "$HERE/relay-update.sh" ports-status; ports_available '3306,13306'; migration_check; log 'migration preflight passed; no changes';;
    install) migration_check; migration_install;;
    verify) lan_rules_check "$(ports_current)";;
    restore)
        private_file "$PORT_HOME/lan-owned" 0
        [ "$(cat "$PORT_HOME/lan-owned")" = lan-v1 ] || fail 'unknown LAN ownership'
        if [ -d "$BACKUP" ]; then
            private_file "$BACKUP/state" 0
            [ "$(cat "$BACKUP/state")" = complete ] || fail 'LAN migration requires manual recovery'
        fi
        if [ -d "$RELAY/transaction" ]; then
            [ -e "$RELAY/transaction/applied" ] || [ -e "$RELAY/transaction/rolled-back" ] || fail 'interrupted deployment; do not alter evidence'
        fi
        acquire_lock "$RELAY"
        trap release_lock EXIT
        lan_rules_apply "$(ports_current)";;
esac
