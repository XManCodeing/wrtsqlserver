#!/bin/sh
# Read-only host updater preflight. Run from a root-owned deployment directory.
# No image required, no mounts, service starts, file creation or Docker mutations.
set -eu
HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
. "$HERE/common.sh"
[ "$#" -eq 0 ] || fail 'usage: updater-preflight.sh'

# trusted_deploy_path(path, kind): validate root-owned, non-writable path ancestry.
# kind file requires a single-link regular deployment file; returns 0 or fails.
trusted_deploy_path() {
    safe_path "$1"
    if [ "$2" = file ]; then
        [ -f "$1" ] && [ "$(file_stat -c %h "$1")" = 1 ] || fail 'unsafe deployment file type/link count'
    else
        [ -d "$1" ] || fail 'deployment directory missing'
    fi
    trust_part=$1
    while :; do
        [ "$(file_stat -c %u "$trust_part")" = 0 ] || fail "deployment path must be root owned: $trust_part"
        case "$(file_stat -c %a "$trust_part")" in 600|640|644|700|750|755) ;; *) fail "unsafe deployment permissions: $trust_part";; esac
        [ "$trust_part" != / ] || break
        trust_part=${trust_part%/*}
        [ -n "$trust_part" ] || trust_part=/
    done
}
# space_check(): validate bounded POSIX df output and require at least 512 MiB free.
# Malformed, duplicate, excessively long or out-of-range numeric output fails closed.
space_check() {
    disk_report=$(df -Pk "$USB") || fail 'cannot inspect USB free space'
    [ "${#disk_report}" -le 8192 ] || fail 'oversized disk space report'
    printf '%s\n' "$disk_report" | awk -v target="$USB" '
      NR==1 {next}
      NR==2 {
        if(NF!=6 || $6!=target || $4 !~ /^[0-9]+$/ || length($4)>15) exit 1
        if($4+0 < 524288) exit 1
        found=1
      }
      NR>2 {bad=1}
      END {exit (!found || bad)}
    ' || fail 'USB needs at least 512 MiB free (valid bounded df -Pk report required)'
}

usb_check
lan_check
for required in wget cmp df date mkdir chmod chown cp mv rm rmdir sync cat id dirname; do
    command -v "$required" >/dev/null || fail "missing command: $required"
done
trusted_deploy_path "$HERE" directory
for deployed_file in common.sh relay-update.sh updater-preflight.sh updater-start.sh; do
    trusted_deploy_path "$HERE/$deployed_file" file
done
if ! command -v stat >/dev/null 2>&1; then
    trusted_deploy_path "$HERE/deploy-stat" file
fi
updater_run_check no
space_check
log 'updater preflight passed: USB/LAN/trusted deployment/runtime/space verified; no changes made'
