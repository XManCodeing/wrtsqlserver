#!/bin/sh
# Usage: sh deploy/host-network-check.sh PROJECT_DIR LAN_IPV4
# Read-only migration checks. Does not restart containers or modify firewall/data.
set -eu

# fail(message): emit a non-secret failure and exit unsuccessfully.
fail() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
[ "$#" -eq 2 ] || fail 'usage: host-network-check.sh PROJECT_DIR LAN_IPV4'
project=$1
address=$2
case "$project" in /*) ;; *) fail 'absolute project directory required';; esac
[ -d "$project" ] && [ ! -L "$project" ] || fail 'real project directory required'
command -v python3 >/dev/null || fail 'python3 required for read-only validation'
command -v docker >/dev/null || fail 'docker required'

# Validate the fixed migration contract without displaying private file contents.
python3 - "$project" "$address" <<'PY'
import ipaddress
import json
import os
from pathlib import Path
import stat
import subprocess
import sys

root = Path(sys.argv[1])
address = ipaddress.IPv4Address(sys.argv[2])
private = ("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16")
if not any(address in ipaddress.IPv4Network(network) for network in private):
    raise SystemExit("LAN IPv4 must be RFC1918")
interfaces = json.loads(subprocess.check_output(["ip", "-j", "-4", "addr", "show"], text=True))
if not any(item.get("local") == str(address) for interface in interfaces for item in interface.get("addr_info", [])):
    raise SystemExit("LAN address is not assigned to this Ubuntu host")
for relative in ("data/relay", "data/redis", "data/secrets", "data/migration"):
    directory = root / relative
    if not directory.is_dir() or str(directory) != os.path.realpath(directory):
        raise SystemExit("Expected real migration directory without symlink ancestors")
for relative in ("data/relay/config.enc", "data/relay/secret.key", "data/secrets/admin_password"):
    path = root / relative
    for component in (path, *path.parents):
        if component.is_symlink():
            raise SystemExit("Symlink rejected in migration data path")
    metadata = path.stat()
    if not stat.S_ISREG(metadata.st_mode) or metadata.st_uid != 10001 or stat.S_IMODE(metadata.st_mode) != 0o600:
        raise SystemExit("Migration files must be regular UID 10001 files with mode 0600")
if (root / "data/relay/secret.key").stat().st_size != 32:
    raise SystemExit("Invalid encryption key size")
for service, expected in (("db-relay", "data/relay"), ("redis-dev", "data/redis")):
    ids = subprocess.check_output(["docker", "--host", "unix:///var/run/docker.sock", "ps", "-aq",
        "--filter", "label=com.docker.compose.project=db-relay",
        "--filter", "label=com.docker.compose.service=" + service], text=True).split()
    if len(ids) != 1:
        raise SystemExit("Expected exactly one existing Compose container per service")
    container = json.loads(subprocess.check_output(["docker", "--host", "unix:///var/run/docker.sock", "inspect", ids[0]], text=True))[0]
    mounts = [mount for mount in container["Mounts"] if mount["Destination"] == "/data"]
    if str(root / expected) != os.path.realpath(root / expected):
        raise SystemExit("Symlink rejected in data directory")
    if len(mounts) != 1 or mounts[0]["Type"] != "bind" or mounts[0]["Source"] != str(root / expected):
        raise SystemExit("Existing data mount differs; migration must stop")
    if not container["State"]["Running"]:
        raise SystemExit("Existing container is not running; diagnose before migration")
print("PASS: LAN address, original credentials and both existing data mounts verified")
PY

printf '\n=== Host TCP listeners (check 6379 before Redis LAN publication) ===\n'
ss -lnt
printf '\n=== UFW status (host-network traffic follows INPUT rules) ===\n'
if command -v ufw >/dev/null 2>&1; then
    ufw status
else
    printf 'UFW not installed; inspect the host firewall separately.\n'
fi
printf '\nNo containers, data or firewall rules were changed.\n'
