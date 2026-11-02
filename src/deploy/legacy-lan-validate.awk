# Strict non-executing recognizer for the confirmed two-rule legacy shell grammar.
# awk -v iface=... -v source=... -v destination=... -v extract=0|1 -f file script
# Only literal rule arguments, "$@", known iptables commands and literal diagnostics
# are accepted. No eval, variable assignment, command substitution or source is allowed.
function reject() { bad=1; exit 1 }
function trim(value) { sub(/^[ \t]+/,"",value); sub(/[ \t]+$/,"",value); return value }
function literal(value) {
    # Diagnostics cannot contain shell expansion, redirection, or command separators.
    return value ~ /^(printf|echo) (["\047][^"\047$`;&|<>]*["\047] ?)+( >&2)?;?$/
}
function rulekind(value, base) {
    if(value ~ /[$`;|&<>\\]/) return 0
    gsub(/["\047]/,"",value)
    base="-i " iface " -s " source " -d " destination " -p tcp "
    if(value==base "--dport 6379 -m comment --comment relay-lan-redis-bypass -j RETURN") return 1
    if(value==base "-m multiport --dports 18080,13306 -m comment --comment relay-lan-app-bypass -j RETURN") return 2
    return 0
}
function command(value, operation, args, kind) {
    sub(/^if /,"",value); sub(/; then$/,"",value); value=trim(value)
    sub(/ 2>\/dev\/null$/,"",value)
    if(value !~ /^iptables (-w 5 )?-t mangle -[CI] PREROUTING /) reject()
    operation=value ~ / -C PREROUTING / ? "C" : "I"
    sub(/^iptables (-w 5 )?-t mangle -[CI] PREROUTING /,"",value)
    if(operation=="I") { if(value !~ /^1 /) reject(); sub(/^1 /,"",value) }
    args=trim(value)
    if(args=="\"$@\"") kind=current
    else kind=rulekind(args)
    if(!kind || kind!=current) reject()
    counts[kind operation]++
}
{
    if(NR==1 && $0!="#!/bin/sh") reject()
    raw[NR]=$0
    line=trim($0)
    if(line=="" || line ~ /^#/) next
    if(pending!="") line=pending " " line
    if(line ~ /\\$/) {sub(/[ \t]*\\$/,"",line); pending=line; next}
    pending=""
    gsub(/[ \t]+/," ",line)
    if(line=="set -eu" || line=="set -e" || line=="set -u") {if(current)reject();next}
    if(line ~ /^set -- /) {
        args=line;sub(/^set -- /,"",args); kind=rulekind(args)
        if(kind!=current+1 || depth) reject()
        current=kind
        if(kind==2) cutoff=statementStart
    } else if(line ~ /^if iptables /) {command(line);depth++}
    else if(line=="then" || line=="else") {if(!depth)reject()}
    else if(line=="fi" || line=="fi;") {if(!depth)reject();depth--}
    else if(line ~ /^iptables /) command(line)
    else if(literal(line)) {if(!depth)reject()}
    else if(line ~ /^exit [01];?$/) {if(!depth)reject()}
    else reject()
    statementStart=NR+1
}
END {
    if(bad || pending!="" || depth || current!=2 || !counts["1C"] || !counts["1I"] || !counts["2C"] || !counts["2I"]) exit 1
    if(extract) for(recordIndex=1;recordIndex<cutoff;recordIndex++) print raw[recordIndex]
}
