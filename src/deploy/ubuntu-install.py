#!/usr/bin/python3
"""Install a prebuilt Ubuntu host updater; optionally enable it on the existing relay.

Root only. --enable-online preserves the host Compose file, relay data and Redis.
--relay-image optionally replaces relay with a verified prebuilt local image and pins
RELAY_IMAGE; otherwise only the socket variable changes. --recover-enable restores
the retained snapshot and original variables. No image is pulled.
"""
import argparse
import fcntl
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import socket
import stat
import subprocess
import sys
import time

SPEC = importlib.util.spec_from_file_location("ubuntu_update", Path(__file__).with_name("ubuntu-update.py"))
UPDATE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(UPDATE)
UNIT = Path("/etc/systemd/system/db-relay-updater.service")
SOCKET = "/run/relay-updater/control.sock"


def trusted(path):
    """Validate an existing root-controlled path and ancestors; return its Path."""
    path = Path(path)
    return UPDATE.trusted(path, directory=path.is_dir())


def directory(path, mode=0o700, group=0):
    """Create/validate an updater directory; enforce mode/group and return None."""
    path.mkdir(mode=mode, exist_ok=True)
    UPDATE.trusted(path, directory=True)
    os.chown(path, 0, group)
    os.chmod(path, mode)


def private_create(path, contents):
    """Exclusively create root-private text at path; sync content and return None."""
    descriptor = os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY | os.O_NOFOLLOW, 0o600)
    try:
        with os.fdopen(descriptor, "w") as stream:
            stream.write(contents)
            stream.flush()
            os.fsync(stream.fileno())
        UPDATE.sync_directory(path.parent)
    except (OSError, RuntimeError):
        path.unlink(missing_ok=True)
        raise


def systemctl(*arguments):
    """Execute bounded systemd operations without leaking output; return None."""
    UPDATE.command(["/usr/bin/systemctl", *arguments])


def validate(args):
    """Validate root inputs and trusted source files before writes; return project/root."""
    UPDATE.require(os.geteuid() == 0, "root required; run with sudo")
    for name in (args.project, args.service):
        UPDATE.require(re.fullmatch(r"[a-z0-9][a-z0-9_-]{0,62}", name), "invalid Compose project/service")
    UPDATE.require(not args.service.startswith("redis"), "Redis cannot be updated")
    UPDATE.current_version_parts(args.initial_version)
    project = trusted(args.project_dir)
    compose = trusted(args.compose_file)
    UPDATE.require(compose.parent == project and re.fullmatch(r"[A-Za-z0-9_./-]+", str(project)),
                   "simple absolute project/Compose paths required")
    UPDATE.require(not compose.name.startswith(".env"), "Compose file cannot be an environment file")
    trusted(project / "data")
    variables = trusted(project / "data/ubuntu.vars")
    UPDATE.require(stat.S_IMODE(variables.stat().st_mode) == 0o600, "data/ubuntu.vars requires root 0600")
    return project, project / "data/updater"


def service_unit(project, root):
    """Render the root host service for trusted project/root paths; return unit text."""
    return f"""[Unit]
Description=DB Relay Ubuntu host updater (Unix socket only)
Requires=docker.service
After=docker.service network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
Group=root
UMask=0077
ExecStart={root}/bin/relay-updater -config {root}/config.json
Restart=on-failure
RestartSec=5
TimeoutStopSec=900
KillMode=control-group
NoNewPrivileges=true
PrivateTmp=true
ProtectHome=true
ProtectSystem=strict
ReadWritePaths={project}/data
RestrictSUIDSGID=true

[Install]
WantedBy=multi-user.target
"""


def wait_ready(root):
    """Wait for active service and correctly permissioned live HTTP socket; return None."""
    endpoint = root / "run/control.sock"
    for _ in range(30):
        if endpoint.exists():
            info = endpoint.lstat()
            UPDATE.require(stat.S_ISSOCK(info.st_mode) and info.st_uid == 0 and info.st_gid == 10001
                           and stat.S_IMODE(info.st_mode) == 0o660, "unsafe updater socket permissions")
            with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as connection:
                connection.settimeout(5)
                connection.connect(str(endpoint))
                connection.sendall(b"GET /status HTTP/1.0\r\nHost: localhost\r\n\r\n")
                response = connection.recv(128)
            UPDATE.require(re.match(rb"HTTP/1\.[01] 200 ", response), "updater status endpoint unavailable")
            systemctl("is-active", "--quiet", UNIT.name)
            return
        time.sleep(1)
    raise RuntimeError("updater socket did not become ready; inspect systemctl status and journalctl")


def install(args):
    """Install prebuilt binary/helper/unit; remove only newly created files on failure."""
    project, root = validate(args)
    binary = UPDATE.trusted(args.binary)
    helper = UPDATE.trusted(Path(__file__).absolute().with_name("ubuntu-update.py"))
    # Refuse obvious wrong-platform binaries without executing untrusted input.
    with binary.open("rb") as stream:
        header = stream.read(20)
    UPDATE.require(header[:6] == b"\x7fELF\x02\x01" and header[18:20] == b"\x3e\x00",
                   "prebuilt Linux amd64 ELF updater binary required")
    config_path = root / "config.json"
    destinations = [root / "bin/relay-updater", root / "bin/ubuntu-update.py", config_path, UNIT]
    UPDATE.require(not any(path.exists() or path.is_symlink() for path in destinations),
                   "existing or partial updater installation retained; inspect it before retrying")
    directory(root)
    directory(root / "docker")
    directory(root / "bin")
    directory(root / "run", 0o750, 10001)
    config = {"hostMode": "ubuntu", "projectDir": str(project), "composeFile": str(args.compose_file),
              "composeProject": args.project, "relayService": args.service,
              "initialVersion": args.initial_version, "deployDir": str(root / "bin")}
    created = []
    try:
        for source, destination, mode in ((binary, destinations[0], 0o755), (helper, destinations[1], 0o644)):
            with destination.open("xb") as output, source.open("rb") as input_file:
                created.append(destination)
                shutil.copyfileobj(input_file, output)
                output.flush()
                os.fsync(output.fileno())
            os.chmod(destination, mode)
        for path, content in ((config_path, json.dumps(config, indent=2) + "\n"), (UNIT, service_unit(project, root))):
            private_create(path, content)
            created.append(path)
        os.chmod(UNIT, 0o644)
        systemctl("daemon-reload")
        systemctl("enable", "--now", UNIT.name)
        wait_ready(root)
    except (RuntimeError, OSError, subprocess.SubprocessError):
        # Never remove a running service's executable or erase retained updater state.
        if UNIT in created and UNIT.exists():
            try:
                systemctl("disable", "--now", UNIT.name)
            except (RuntimeError, OSError, subprocess.SubprocessError):
                raise RuntimeError("installation failed; service cleanup failed; retained files require manual inspection") from None
        for path in reversed(created):
            path.unlink(missing_ok=True)
        systemctl("daemon-reload")
        raise
    print("Host updater installed and socket ready; Redis and relay containers unchanged", flush=True)


def socket_variables(contents):
    """Replace exactly one disabled socket variable, preserving all other lines; return bytes."""
    text = contents.decode("utf-8")
    matches = list(re.finditer(r"(?m)^ONLINE_UPDATE_SOCKET=([^\r\n]*)", text))
    UPDATE.require(len(matches) == 1 and matches[0].group(1).strip() in ("", "''", '\"\"'),
                   "ubuntu.vars must contain exactly one disabled ONLINE_UPDATE_SOCKET setting")
    match = matches[0]
    return (text[:match.start()] + "ONLINE_UPDATE_SOCKET=" + SOCKET + text[match.end():]).encode()


def relay_image_variables(contents, image_id):
    """Pin exactly one RELAY_IMAGE variable to validated immutable ID; return updated bytes."""
    UPDATE.require(re.fullmatch(r"sha256:[0-9a-f]{64}", image_id), "invalid local relay image ID")
    text = contents.decode("utf-8")
    matches = list(re.finditer(r"(?m)^RELAY_IMAGE=([^\r\n]*)", text))
    UPDATE.require(len(matches) == 1, "ubuntu.vars must contain exactly one RELAY_IMAGE setting")
    match = matches[0]
    return (text[:match.start()] + "RELAY_IMAGE=" + image_id + text[match.end():]).encode()


def target_image(reference, initial_version):
    """Validate prebuilt local reference/platform/contracts/version; return pinned ID and version."""
    UPDATE.require(re.fullmatch(r"[a-z0-9][a-z0-9._:/@-]{0,254}", reference),
                   "invalid local relay image reference")
    image = json.loads(UPDATE.command(UPDATE.DOCKER + ["image", "inspect", reference]))[0]
    labels = image["Config"].get("Labels") or {}
    version = labels.get("org.opencontainers.image.version", "")
    UPDATE.current_version_parts(version)
    UPDATE.require(image.get("Os") == "linux" and image.get("Architecture") == "amd64"
                   and labels.get("io.db-relay.runtime") == UPDATE.CONTRACT
                   and labels.get("io.db-relay.network.host") == "v1", "local relay image contract rejected")
    UPDATE.require(version == initial_version or UPDATE.release_newer(version, initial_version),
                   "local relay image must preserve initial version or safely upgrade it")
    UPDATE.require(re.fullmatch(r"sha256:[0-9a-f]{64}", image["Id"]), "invalid local relay image ID")
    return image["Id"], version


def recover(adapter):
    """Restore retained original variables/image/data; keep evidence and return None."""
    UPDATE.trusted(adapter.transaction, True)
    UPDATE.require(not any(adapter.root.glob("archive-*")),
                   "online update history exists; initial enable recovery is no longer safe")
    if adapter.active.exists():
        active = UPDATE.read_json(adapter.active)
        allowed = [UPDATE.read_json(adapter.transaction / name) for name in ("previous.json", "next.json")
                   if (adapter.transaction / name).exists()]
        UPDATE.require(active in allowed, "active deployment changed since initial enablement; refuse stale recovery")
    previous = UPDATE.trusted(adapter.transaction / "ubuntu.vars")
    UPDATE.durable(adapter.variables, previous.read_bytes())
    adapter.rollback()
    # Before initial enablement there was no active override; restore that exact state.
    adapter.active.unlink(missing_ok=True)
    UPDATE.sync_directory(adapter.root)
    print("Original relay variables, image and data restored; Redis untouched", flush=True)


def activate(args, adapter):
    """Preflight current relay, install updater, snapshot and enable socket; return None."""
    UPDATE.require(not adapter.transaction.exists() and not adapter.active.exists()
                   and not (adapter.root / "transaction").exists(), "retained updater state requires inspection/recovery")
    adapter.preflight()
    UPDATE.require(adapter.current() == args.initial_version, "running version differs from initial version")
    container = adapter.container()
    config = adapter.configuration()
    resolved_image = config["services"][args.service]["image"]
    identity = json.loads(UPDATE.command(UPDATE.DOCKER + ["image", "inspect", resolved_image]))[0]
    UPDATE.require(identity["Id"] == container["Image"], "Compose image differs from the running relay image")
    original = adapter.variables.read_bytes()
    enabled = socket_variables(original)
    image_id, expected_version = container["Image"], args.initial_version
    if args.relay_image:
        image_id, expected_version = target_image(args.relay_image, args.initial_version)
        enabled = relay_image_variables(enabled, image_id)
    service = config["services"][args.service]
    UPDATE.require(service.get("environment", {}).get("ONLINE_UPDATE_SOCKET", "") == "",
                   "resolved Compose socket must be disabled before enabling")
    install(args)
    adapter.transaction.mkdir(mode=0o700)
    UPDATE.sync_directory(adapter.root)
    rollback_tag = "local/db-relay-rollback:enable-" + str(time.time_ns())
    UPDATE.command(UPDATE.DOCKER + ["tag", container["Image"], rollback_tag])
    service["image"] = rollback_tag
    UPDATE.durable(adapter.transaction / "previous.json", json.dumps(config).encode())
    UPDATE.durable(adapter.transaction / "image-id", container["Image"].encode())
    UPDATE.durable(adapter.transaction / "ubuntu.vars", original)
    try:
        print("Stopping only db-relay for a consistent data snapshot", flush=True)
        adapter.stop()
        UPDATE.command(["/usr/bin/cp", "-a", "--reflink=auto", "--", str(adapter.data),
                        str(adapter.transaction / "data")])
        UPDATE.command(["/usr/bin/sync", "-f", str(adapter.transaction)])
        UPDATE.durable(adapter.transaction / "backup-ready", b"yes")
        UPDATE.durable(adapter.variables, enabled)
        service["image"] = image_id
        service.setdefault("environment", {})["ONLINE_UPDATE_SOCKET"] = SOCKET
        UPDATE.durable(adapter.transaction / "next.json", json.dumps(config).encode())
        adapter.up(adapter.transaction / "next.json")
        UPDATE.require(adapter.current() == expected_version, "enabled relay version differs from validated image")
        UPDATE.require(adapter.container()["Image"] == image_id, "enabled relay image differs from pinned image")
        UPDATE.durable(adapter.active, (adapter.transaction / "next.json").read_bytes())
        UPDATE.durable(adapter.transaction / "applied", b"yes")
    except (RuntimeError, OSError, subprocess.SubprocessError, ValueError, KeyError):
        try:
            recover(adapter)
        except (RuntimeError, OSError, subprocess.SubprocessError, ValueError, KeyError):
            raise RuntimeError("enable failed and recovery incomplete; rerun with --recover-enable; retained snapshot must not be removed") from None
        raise RuntimeError("enable failed; original relay restored; updater remains installed") from None
    print("Online updates enabled on the verified local image; host Compose/data/Redis preserved", flush=True)


def main():
    """Parse host CLI; serialize initial activation with the updater transaction lock."""
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("project-dir", "compose-file", "project", "service", "initial-version"):
        parser.add_argument("--" + name, required=True)
    parser.add_argument("--binary")
    parser.add_argument("--relay-image", default="", help="prebuilt local relay image for first online enablement only")
    modes = parser.add_mutually_exclusive_group()
    modes.add_argument("--enable-online", action="store_true")
    modes.add_argument("--recover-enable", action="store_true")
    args = parser.parse_args()
    if args.relay_image and not args.enable_online:
        parser.error("--relay-image requires --enable-online")
    if not args.recover_enable and not args.binary:
        parser.error("--binary is required unless --recover-enable is used")
    validate(args)
    if not args.enable_online and not args.recover_enable:
        install(args)
        return
    adapter = UPDATE.Adapter(args)
    adapter.transaction = adapter.root / "enable-transaction"
    descriptor = os.open(adapter.root / "transaction.lock", os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    try:
        fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        if args.recover_enable:
            UPDATE.require(not (adapter.root / "transaction").exists(), "online update transaction exists; do not restore initial enable snapshot")
            recover(adapter)
        else:
            activate(args, adapter)
    finally:
        os.close(descriptor)


if __name__ == "__main__":
    try:
        main()
    except RuntimeError as error:
        print(f"Ubuntu updater installation failed: {error}", file=sys.stderr)
        sys.exit(1)
    except (OSError, subprocess.SubprocessError, ValueError, KeyError) as error:
        print(f"Ubuntu updater installation failed ({type(error).__name__}); inspect trusted paths/service and retained transaction", file=sys.stderr)
        sys.exit(1)
