#!/bin/sh
# Install the opt-in Ubuntu LAN firewall service from a verified, root-owned release.
# Usage: sudo sh install-ubuntu-firewall.sh. Does not restart Docker, db-relay or Redis.
set -eu
umask 077

# fail(message): stop before installing untrusted inputs; output no secret values.
fail() { printf 'ERROR: %s\n' "$1" >&2; exit 1; }
[ "$(id -u)" = 0 ] || fail 'root required'
source_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
helper=ubuntu-firewall.py
unit=db-relay-firewall
lib=/usr/local/lib/db-relay
units=/etc/systemd/system

# trusted(path): require an existing root-controlled entry and every ancestor.
trusted() {
    entry=$1
    while :; do
        [ ! -L "$entry" ] && [ "$(stat -c %u -- "$entry")" = 0 ] || fail "untrusted path: $entry"
        mode=$(stat -c %a -- "$entry")
        [ $((0$mode & 022)) -eq 0 ] || fail "group/other writable path: $entry"
        [ "$entry" != / ] || break
        entry=$(dirname -- "$entry")
    done
}
for file in "$helper" "$unit.service" "$unit.timer" "$unit.path"; do
    [ -f "$source_dir/$file" ] || fail "missing release file: $file"
    trusted "$source_dir/$file"
done
trusted /usr/local/lib
trusted "$units"
[ -x /usr/sbin/ufw ] && [ -x /usr/bin/docker ] || fail 'UFW and Docker are required'
LC_ALL=C /usr/sbin/ufw status | grep -qx 'Status: active' || fail 'UFW is inactive; no firewall changes made'
[ ! -e "$lib" ] && [ ! -L "$lib" ] || trusted "$lib"
# configure_lan(): derive scope from trusted deployment config and the live interface.
# No shell sourcing: ubuntu.vars is data, and only LAN_BIND_IP is interpreted.
configure_lan() {
    project=/www/wwwroot/db-relay
    config=/etc/db-relay-firewall.conf
    trusted "$project/data/ubuntu.vars"
    if [ -e "$config" ] || [ -L "$config" ]; then
        trusted "$config"
    fi
    /usr/bin/python3 -I -B - "$project" "$config" <<'PY'
import ipaddress
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile

project, config = map(Path, sys.argv[1:])
addresses = [line.removeprefix("LAN_BIND_IP=") for line in
             (project / "data/ubuntu.vars").read_text().splitlines() if line.startswith("LAN_BIND_IP=")]
if len(addresses) != 1:
    sys.exit("expected one LAN_BIND_IP in deployment variables")
address = ipaddress.IPv4Address(addresses[0])
interfaces = json.loads(subprocess.check_output(["/usr/sbin/ip", "-j", "-4", "address", "show", "up"], timeout=10))
networks = [ipaddress.IPv4Network((address, item["prefixlen"]), strict=False)
            for interface in interfaces for item in interface.get("addr_info", [])
            if item.get("local") == str(address) and item.get("scope") == "global"]
private = list(map(ipaddress.IPv4Network, ("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16")))
if len(networks) != 1 or not any(networks[0].subnet_of(block) for block in private):
    sys.exit("LAN_BIND_IP must belong to one active private IPv4 interface")
contents = f"RELAY_PROJECT_DIR={project}\nRELAY_LAN_IP={address}\nRELAY_LAN_CIDR={networks[0]}\n"
if config.exists():
    if config.read_text() != contents:
        sys.exit("existing firewall LAN configuration differs; inspect it before reinstalling")
else:
    with tempfile.NamedTemporaryFile(dir=config.parent, delete=False, mode="w") as stream:
        try:
            stream.write(contents)
            stream.flush()
            os.fsync(stream.fileno())
            os.link(stream.name, config)
        finally:
            os.unlink(stream.name)
print(f"db-relay firewall scope: {networks[0]} -> {address}")
PY
}
configure_lan
install -d -o root -g root -m 0755 "$lib"
for dest in "$lib/$helper" "$units/$unit.service" "$units/$unit.timer" "$units/$unit.path"; do
    [ ! -e "$dest" ] && [ ! -L "$dest" ] || trusted "$dest"
done
install -o root -g root -m 0644 "$source_dir/$helper" "$lib/$helper"
install -o root -g root -m 0644 "$source_dir/$unit.service" "$source_dir/$unit.timer" "$source_dir/$unit.path" "$units/"
systemctl daemon-reload
systemctl start "$unit.service"
systemctl enable --now "$unit.path" "$unit.timer"
printf '%s\n' 'db-relay LAN firewall reconciliation enabled. Inspect journalctl -u db-relay-firewall.service for failures.'
