#!/bin/sh
# Owned direct PREROUTING RETURN rules: a child-chain RETURN would NOT bypass proxy jumps.
LAN_TAG=db-relay-lan-v1

# lan_scope(): derive the actual LAN prefix from the confirmed interface; no public/all-source rule.
lan_scope() {
    LAN_CIDR=$(ip -4 addr show dev "$LAN_INTERFACE" | awk -v wanted="$LAN_BIND_IP" '$1=="inet" {split($2,parts,"/"); if(parts[1]==wanted) {print $2; count++}} END {if(count!=1) exit 1}') || fail 'ambiguous LAN prefix'
    printf '%s\n' "$LAN_CIDR" | awk -F/ 'NF!=2 || $2<8 || $2>30 {exit 1}' || fail 'unsupported LAN prefix'
    # iptables canonicalizes -s, so calculate the network address for strict rule comparison.
    LAN_CIDR=$(printf '%s\n' "$LAN_CIDR" | awk -F'[./]' '{bits=$5; for(i=1;i<=4;i++){take=bits>8?8:bits; if(take<0)take=0; block=2^(8-take); octet=int($i/block)*block; printf "%s%d",(i==1?"":"."),octet; bits-=8} printf "/%d\n",$5}')
}
# lan_rule(action, table, port): execute one exact owned LAN-only direct RETURN rule.
lan_rule() {
    action=$1; table=$2; rule_port=$3
    if [ "$action" = -I ]; then
        set -- -I PREROUTING 1
    else
        set -- "$action" PREROUTING
    fi
    iptables -t "$table" "$@" -i "$LAN_INTERFACE" -s "$LAN_CIDR" -d "$LAN_BIND_IP/32" -p tcp -m tcp --dport "$rule_port" -m comment --comment "$LAN_TAG" -j RETURN
}
# lan_owned_validate(table): reject malformed/foreign rules bearing our exact ownership tag.
lan_owned_validate() {
    owned_rules=$(iptables -t "$1" -S PREROUTING) || fail 'cannot read PREROUTING'
    printf '%s\n' "$owned_rules" | awk -v tag="$LAN_TAG" -v iface="$LAN_INTERFACE" -v src="$LAN_CIDR" -v dst="$LAN_BIND_IP/32" '
      index($0,tag) {
        gsub(/"/, "");
        if(NF!=20 || $1!="-A" || $2!="PREROUTING" || $3!="-s" || $4!=src || $5!="-d" || $6!=dst || $7!="-i" || $8!=iface || $9!="-p" || $10!="tcp" || $11!="-m" || $12!="tcp" || $13!="--dport" || $15!="-m" || $16!="comment" || $17!="--comment" || $18!=tag || $19!="-j" || $20!="RETURN") exit 1
        port=$14; if(port!=18080 && (port<1024 || port>65535 || port==6379 || port==8080)) exit 1
        if(seen[port]++) exit 1
      }' || fail 'unknown rule using project ownership marker'
}
# lan_rules_check(csv): require exact owned rules ahead of every other PREROUTING rule.
lan_rules_check() {
    ports_validate "$1"; lan_scope
    expected_rule_ports=$(printf '%s\n' "18080,$1" | tr ',' '\n' | awk 'NF' | sort -n)
    for rule_table in mangle; do
        lan_owned_validate "$rule_table"
        observed_rule_ports=$(printf '%s\n' "$owned_rules" | awk -v tag="$LAN_TAG" '$1=="-A" {if(index($0,tag)){if(foreign)exit 1; for(i=1;i<=NF;i++)if($i=="--dport")print $(i+1)}else foreign=1}' | sort -n)
        [ "$observed_rule_ports" = "$expected_rule_ports" ] || fail 'LAN exceptions missing, unexpected or behind another PREROUTING jump'
        # A separate order check must not be hidden by a pipeline sort exit status.
        printf '%s\n' "$owned_rules" | awk -v tag="$LAN_TAG" '$1=="-A" {if(index($0,tag)){if(foreign)exit 1}else foreign=1}' || fail 'LAN exception order changed; inspect firewall integration'
    done
}
# lan_rules_apply(csv): modify exact owned rules only, leaving Redis and all proxy rules untouched.
lan_rules_apply() {
    ports_validate "$1"; lan_scope
    requested_rules=$1
    for rule_table in mangle; do
        lan_owned_validate "$rule_table"
        old_rule_ports=$(printf '%s\n' "$owned_rules" | awk -v tag="$LAN_TAG" 'index($0,tag){for(i=1;i<=NF;i++)if($i=="--dport")print $(i+1)}')
        for rule_port in $old_rule_ports; do lan_rule -D "$rule_table" "$rule_port"; done
        for rule_port in 18080 $(printf '%s' "$requested_rules" | tr ',' ' '); do lan_rule -I "$rule_table" "$rule_port"; done
    done
    lan_rules_check "$requested_rules"
}
