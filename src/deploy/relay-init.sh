#!/bin/sh
# Manual usage: LAN_INTERFACE=br-lan LAN_BIND_IP=<LAN IPv4> \
# LAN_INTERFACE_CONFIRMED=yes RELAY_IMAGE=local/db-relay:1.0.0 \
# ADMIN_PASSWORD_SOURCE=/absolute/root-owned-0600-file sh relay-init.sh [check|start]
# Default check is read-only. Password source must contain >=12 non-whitespace
# bytes, is never printed, and is copied into an independent USB secret file.
# HTTP management is plaintext: trusted LAN only. Port 13306 is published but
# mapping must be configured in UI (container listen 0.0.0.0:13306, explicit ACL).
# No automatic upstream/proxy settings or secret generation is performed.
set -eu
HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
. "$HERE/common.sh"
MODE=${1:-check}
[ "$#" -le 1 ] || fail 'usage: relay-init.sh [check|start]'
case "$MODE" in check|start) ;; *) fail 'usage: relay-init.sh [check|start]';; esac
usb_check
lan_check
command -v wget >/dev/null || fail 'wget required'
image_check "${RELAY_IMAGE:-local/db-relay:1.0.0}"
require_absent "$NAME"
require_absent "$OLD"
safe_path "$RELAY"
[ ! -e "$RELAY" ] || fail 'existing relay directory: initialization refuses overwrite'
[ -n "${ADMIN_PASSWORD_SOURCE:-}" ] || fail 'set ADMIN_PASSWORD_SOURCE to an existing private file'
private_file "$ADMIN_PASSWORD_SOURCE" 0
password_parent=${ADMIN_PASSWORD_SOURCE%/*}
[ -n "$password_parent" ] || password_parent=/
private_dir "$password_parent" 0
[ "$(tr -d '[:space:]' < "$ADMIN_PASSWORD_SOURCE" | wc -c)" -ge 12 ] || fail 'administrator password must contain at least 12 non-whitespace bytes'
log 'relay preflight passed; no writes performed'
[ "$MODE" = start ] || exit 0
new_dir "$RELAY" 0
new_dir "$RELAY/data" 10001
cp "$ADMIN_PASSWORD_SOURCE" "$RELAY/admin_password"
chown 10001:10001 "$RELAY/admin_password"
chmod 600 "$RELAY/admin_password"
log 'starting relay; no Redis operations; partial initialization retained on failure'
relay_run "$IMAGE_ID"
relay_health || fail 'relay started but health check failed; inspect manually (no data removed)'
log 'relay HTTP session health passed; configure forwarding/ACL in UI separately'
