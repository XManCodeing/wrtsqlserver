#!/bin/sh
# Sourced host port policy helpers. No effects occur merely by sourcing this file.
PORT_HOME=$USB/relay-updater
PORT_POLICY=$PORT_HOME/ports.applied

# ports_validate(csv): require a canonical sorted bounded business-port set; empty is valid.
ports_validate() {
    [ "$(printf '%s' "$1" | wc -l | tr -d ' ')" = 0 ] || fail 'multiline port list rejected'
    printf '%s\n' "$1" | awk -F, '
      $0=="" {exit 0}
      NF>64 {exit 1}
      {for(i=1;i<=NF;i++) {
        if($i !~ /^[1-9][0-9]*$/ || $i<1024 || $i>65535 || $i==6379 || $i==8080 || $i==18080 || (i>1 && $i<=$(i-1))) exit 1
      }}' || fail 'ports must be sorted unique 1024..65535; reserved: 6379,8080,18080; maximum 64'
}
# ports_read(path): read one validated private CSV policy; emits only its canonical value.
ports_read() {
    private_file "$1" 0
    port_value=$(cat "$1")
    [ "$(wc -l < "$1" | tr -d ' ')" = 1 ] || fail 'port policy must have exactly one line'
    ports_validate "$port_value"
    printf '%s\n' "$port_value"
}
# ports_current(): legacy baseline is allowed only until an explicit migration creates policy.
ports_current() {
    safe_path "$PORT_POLICY"
    if [ -e "$PORT_POLICY" ]; then
        ports_read "$PORT_POLICY"
    else
        legacy_bindings=$(docker_local inspect --format '{{range $port, $bindings := .HostConfig.PortBindings}}{{range $bindings}}{{.HostIp}}:{{.HostPort}}:{{$port}};{{end}}{{end}}' "${1:-$NAME}")
        legacy_bindings=$(printf '%s' "$legacy_bindings" | tr ';' '\n' | sort | tr '\n' ';')
        case "$legacy_bindings" in
            "$LAN_BIND_IP:13306:13306/tcp;$LAN_BIND_IP:18080:8080/tcp;") printf '13306\n';;
            "$LAN_BIND_IP:13306:13306/tcp;$LAN_BIND_IP:18080:8080/tcp;$LAN_BIND_IP:3306:3306/tcp;") printf '3306,13306\n';;
            *) fail 'unknown initial published port layout; EXPOSE is not publication';;
        esac
    fi
}
# ports_save(csv): atomically commit the host policy after validating the private USB parent.
ports_save() {
    ports_validate "$1"
    private_dir "$PORT_HOME" 0
    safe_path "$PORT_POLICY"
    [ ! -e "$PORT_POLICY" ] || private_file "$PORT_POLICY" 0
    policy_temporary=$PORT_HOME/.ports-applied-$$
    safe_path "$policy_temporary"
    (set -C; printf '%s\n' "$1" > "$policy_temporary")
    sync
    mv "$policy_temporary" "$PORT_POLICY"
    sync
}
# ports_layout(name): choose the original/candidate snapshot by exact journal identity.
# Never validate a retained old container against the newly applied policy.
ports_layout() {
    if [ -d "$TX" ] && [ -e "$TX/ports-original" ]; then
        original_ports=$(ports_read "$TX/ports-original")
        candidate_ports=$(ports_read "$TX/ports-candidate")
        layout_id=$(full_id "$1")
        if [ "$layout_id" = "$(original_id)" ]; then
            expected_ports=$original_ports
        else
            private_file "$TX/candidate-id" 0
            [ "$layout_id" = "$(cat "$TX/candidate-id")" ] || fail 'unknown port transaction container'
            expected_ports=$candidate_ports
        fi
    else
        expected_ports=$(ports_current "$1")
    fi
    expected_bindings=$(printf '%s\n' "$expected_ports" | awk -F, -v ip="$LAN_BIND_IP" '
      {print ip ":18080:8080/tcp"; if($0!="") for(i=1;i<=NF;i++) print ip ":" $i ":" $i "/tcp"}' | sort)
    actual_bindings=$(printf '%s' "$ports" | tr ';' '\n' | sort)
    [ "$actual_bindings" = "$expected_bindings" ] || fail 'LAN/ports differ from managed policy or transaction snapshot'
}
# ports_available(csv): reject newly requested host listeners and other Docker publications.
# Existing relay bindings are permitted; listening IPv4/IPv6 wildcard ports are not stolen.
ports_available() {
    if command -v ss >/dev/null; then
        listeners=$(ss -H -ltn) || fail 'cannot inspect host listeners'
    elif command -v netstat >/dev/null; then
        listeners=$(netstat -lnt) || fail 'cannot inspect host listeners'
    else
        fail 'ss or netstat required for port conflict preflight'
    fi
    publications=$(docker_local container ls --format '{{.Names}} {{.Ports}}') || fail 'cannot inspect Docker publications'
    existing_ports=$(ports_current)
    for requested_port in $(printf '%s' "$1" | tr ',' ' '); do
        case ",$existing_ports," in *,"$requested_port",*) continue;; esac
        printf '%s\n' "$listeners" | awk -v port="$requested_port" '$4 ~ (":" port "$") {found=1} END {exit found}' || fail "host port already in use: $requested_port"
        printf '%s\n' "$publications" | awk -v port="$requested_port" '$1!="db-relay" && $0 ~ (":" port "->") {found=1} END {exit found}' || fail "Docker port already published: $requested_port"
    done
}
# ports_preflight(): first migration retains both historical 13306 and explicitly adds 3306.
ports_preflight() {
    if [ -d "$PORT_HOME/lan-migration" ]; then
        private_file "$PORT_HOME/lan-migration/state" 0
        [ "$(cat "$PORT_HOME/lan-migration/state")" = complete ] || fail 'LAN migration requires manual recovery'
    fi
    ports_validate "$RELAY_PORTS"
    private_file "$PORT_HOME/lan-owned" 0
    [ "$(cat "$PORT_HOME/lan-owned")" = lan-v1 ] || fail 'LAN migration must be explicitly installed first'
    if [ -e "$PORT_HOME/ports-migrated" ]; then
        private_file "$PORT_HOME/ports-migrated" 0
        [ "$(cat "$PORT_HOME/ports-migrated")" = yes ] || fail 'unknown migration marker'
    else
        case ",$RELAY_PORTS," in *,3306,*) ;; *) fail 'first migration must explicitly include 3306';; esac
        case ",$RELAY_PORTS," in *,13306,*) ;; *) fail 'first migration must retain 13306';; esac
    fi
    lan_rules_check "$(ports_current)"
    if [ -e "$TX/lan-managed" ]; then
        private_file "$TX/lan-mangle-before" 0
    fi
    ports_available "$RELAY_PORTS"
}
# ports_snapshot(): preserve policies before stop; rule text is retained for manual evidence.
ports_snapshot() {
    printf '%s\n' "$(ports_current)" > "$TX/ports-original"
    printf '%s\n' "$RELAY_PORTS" > "$TX/ports-candidate"
    if [ -e "$PORT_HOME/lan-owned" ]; then
        lan_rules_check "$(ports_current)"
        printf 'yes\n' > "$TX/lan-managed"
        iptables -t mangle -S PREROUTING > "$TX/lan-mangle-before"
    fi
    sync
}
# ports_commit(): reconcile only owned rules then persist the active canonical policy.
ports_commit() {
    [ ! -e "$TX/lan-managed" ] || lan_rules_apply "$RELAY_PORTS"
    ports_save "$RELAY_PORTS"
}
# restore_ports_policy(): verify restored policies have trusted snapshots before touching rules.
restore_ports_policy() {
    private_file "$TX/ports-original" 0
    private_file "$TX/ports-candidate" 0
    if [ -e "$TX/lan-managed" ]; then
        private_file "$TX/lan-managed" 0
        [ "$(cat "$TX/lan-managed")" = yes ] || fail 'invalid LAN restoration marker'
        private_file "$TX/lan-mangle-before" 0
    fi
}
# ports_restore(): restore prior owned rules/policy after original container identity is confirmed.
ports_restore() {
    [ -e "$TX/ports-original" ] || return 0
    restore_ports_policy
    restore_ports=$(ports_read "$TX/ports-original")
    [ ! -e "$TX/lan-managed" ] || lan_rules_apply "$restore_ports"
    ports_save "$restore_ports"
}
