#!/bin/sh
# fw3 include: strictly parse inert host scope after verifying the actual USB mount.
set -eu
HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
. "$HERE/common.sh"
usb_check
SCOPE=$USB/relay-updater/lan-scope
private_file "$SCOPE" 0
# Reject duplicates, unknown fields, shell syntax and oversized documents before assigning values.
[ "$(wc -c < "$SCOPE" | tr -d ' ')" -le 256 ] || fail 'oversized LAN scope'
awk -F= '
  NR==1 {if(NF!=2 || $1!="LAN_INTERFACE" || $2 !~ /^[a-zA-Z0-9_][a-zA-Z0-9_.:-]*$/ || length($2)>64) exit 1; next}
  NR==2 {if(NF!=2 || $1!="LAN_BIND_IP" || $2 !~ /^[0-9.]+$/) exit 1; next}
  NR>2 {exit 1}
  END {if(NR!=2) exit 1}
' "$SCOPE" || fail 'invalid LAN scope data; shell content is never executed'
LAN_INTERFACE=$(awk -F= 'NR==1 {print $2}' "$SCOPE")
LAN_BIND_IP=$(awk -F= 'NR==2 {print $2}' "$SCOPE")
LAN_INTERFACE_CONFIRMED=yes
export LAN_INTERFACE LAN_BIND_IP LAN_INTERFACE_CONFIRMED
lan_check
exec sh "$HERE/lan-manage.sh" restore
