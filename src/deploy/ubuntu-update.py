#!/usr/bin/python3
"""Root-only Ubuntu Compose adapter. No shell, OpenWrt state or external dependencies.

Every Docker mutation is pinned to the configured project and relay service. Release
images must declare io.db-relay.runtime=portable-v1 and a stable OCI version.
Transactions retain a stopped-service data snapshot, resolved Compose configuration,
and a local rollback image tag; neither Redis nor unrelated containers are touched.
"""
import argparse
import fcntl
import json
import os
from pathlib import Path
import re
import shutil
import stat
import subprocess
import sys
import time

DOCKER = ["/usr/bin/docker", "--host", "unix:///var/run/docker.sock"]
CONTRACT = "portable-v1"
VERSION = re.compile(r"^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$")


def require(condition, message):
    """Raise a static safe diagnostic when a policy condition fails; return None."""
    if not condition:
        raise RuntimeError(message)


def current_version_parts(version):
    """Parse stable or exact bootstrap identity; return numeric core and prerelease flag."""
    bootstrap = version == "1.2.3-ubuntu"
    core = "1.2.3" if bootstrap else version
    require(VERSION.fullmatch(core), "stable version or exact 1.2.3-ubuntu identity required")
    parts = tuple(map(int, core.split(".")))
    require(all(part <= 18446744073709551615 for part in parts), "version component exceeds uint64")
    return parts, bootstrap


def release_newer(candidate, current):
    """Compare stable candidate to approved current identity; return safe upgrade eligibility."""
    require(VERSION.fullmatch(candidate), "stable release candidate required")
    candidate_parts, _ = current_version_parts(candidate)
    current_parts, bootstrap = current_version_parts(current)
    return candidate_parts > current_parts or (bootstrap and candidate_parts == current_parts)


def trusted(path, directory=False):
    """Validate an absolute root-owned non-writable path and ancestors; return Path."""
    path = Path(path)
    require(path.is_absolute() and str(path) == os.path.normpath(path), "unsafe path")
    for entry in [path, *path.parents]:
        info = entry.lstat()
        require(info.st_uid == 0 and not stat.S_ISLNK(info.st_mode)
                and not info.st_mode & 0o022,
                f"untrusted deployment path: {entry}; requires root ownership, no symlink or group/other write")
    require(path.is_dir() if directory else path.is_file(), "invalid deployment path type")
    return path


def command(arguments):
    """Execute fixed argument arrays with bounded time; never disclose stderr or secrets."""
    result = subprocess.run(arguments, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                            timeout=110, check=False)
    require(result.returncode == 0, "host command failed")
    return result.stdout.decode("utf-8")


def durable(path, content):
    """Atomically save private bytes and sync their parent; return None."""
    temporary = path.with_name(path.name + ".tmp-" + str(time.time_ns()))
    descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    try:
        with os.fdopen(descriptor, "wb") as stream:
            stream.write(content)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
        sync_directory(path.parent)
    finally:
        # A full disk or interrupted write must not block a subsequent rollback write.
        temporary.unlink(missing_ok=True)


def sync_directory(path):
    """Flush directory metadata to durable storage; return None."""
    descriptor = os.open(path, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def read_json(path):
    """Read a trusted root-private transaction document; return its parsed object."""
    trusted(path)
    require(path.stat().st_mode & 0o777 == 0o600, "private transaction file required")
    return json.loads(path.read_text())


class Adapter:
    """Hold validated host configuration and one relay-only Compose transaction."""

    def __init__(self, args):
        """Validate host-only CLI arguments and derive fixed paths; no container writes."""
        require(os.geteuid() == 0, "host root required")
        self.args = args
        self.project = trusted(args.project_dir, True)
        self.compose_file = trusted(args.compose_file)
        require(self.compose_file.parent == self.project, "Compose file outside project")
        for name in (args.project, args.service):
            require(re.fullmatch(r"[a-z0-9][a-z0-9_-]{0,62}", name), "invalid Compose name")
        require(not args.service.startswith("redis"), "Redis cannot be updated")
        current_version_parts(args.initial_version)
        self.root = trusted(self.project / "data/updater", True)
        self.transaction = self.root / "transaction"
        self.data = self.project / "data/relay"
        self.active = self.root / "active-compose.json"
        source = self.project / "data/ubuntu.vars"
        self.variables = trusted(source)
        require(self.variables.stat().st_mode & 0o777 == 0o600, "Compose variables require root 0600")
        self.base = DOCKER + ["compose", "--project-directory", str(self.project),
                              "--env-file", str(self.variables), "-p", args.project]

    def compose(self, path, *arguments):
        """Run Compose against exactly one explicit configuration; return stdout."""
        return command(self.base + ["-f", str(path), *arguments])

    def container(self, running=True):
        """Resolve one labelled relay container only; reject identity or mount mismatch."""
        output = command(DOCKER + ["ps", "-aq", "--filter",
                         "label=com.docker.compose.project=" + self.args.project,
                         "--filter", "label=com.docker.compose.service=" + self.args.service])
        identifiers = output.split()
        require(len(identifiers) == 1 and re.fullmatch(r"[0-9a-f]{12,64}", identifiers[0]),
                "exactly one managed relay container required")
        container = json.loads(command(DOCKER + ["inspect", identifiers[0]]))[0]
        labels = container["Config"].get("Labels") or {}
        require(labels.get("com.docker.compose.project") == self.args.project
                and labels.get("com.docker.compose.service") == self.args.service
                and labels.get("com.docker.compose.project.working_dir") == str(self.project)
                and labels.get("io.db-relay.runtime") == CONTRACT,
                "container must satisfy portable-v1 deployment contract")
        require(container["HostConfig"]["NetworkMode"] == "host", "relay host network required")
        image = json.loads(command(DOCKER + ["image", "inspect", container["Image"]]))[0]
        image_labels = image["Config"].get("Labels") or {}
        require(image_labels.get("io.db-relay.runtime") == CONTRACT,
                "running image lacks portable-v1 runtime contract")
        require(image_labels.get("org.opencontainers.image.version", "")
                == labels.get("org.opencontainers.image.version", ""),
                "container and image OCI version labels differ")
        if running:
            require(container["State"]["Running"], "relay is not running")
        self.validate_mounts(container["Mounts"])
        return container

    def validate_mounts(self, mounts):
        """Allow only relay data, readonly password and readonly updater socket bind mounts."""
        expected = {"/data": (str(self.data), True),
                    "/run/secrets/admin_password": (str(self.project / "data/secrets/admin_password"), False),
                    "/run/relay-updater": (str(self.root / "run"), False)}
        actual = {}
        for mount in mounts:
            if mount["Type"] == "tmpfs":
                continue
            require(mount["Type"] == "bind", "unexpected relay mount")
            actual[mount["Destination"]] = (mount["Source"], mount["RW"])
        require(actual == expected, "relay bind mounts violate deployment contract")

    def current(self):
        """Return exact stable/bootstrap OCI identity; missing labels use root-approved initial identity."""
        labels = self.container()["Config"]["Labels"]
        version = labels.get("org.opencontainers.image.version", "") or self.args.initial_version
        current_version_parts(version)
        return version

    def configuration(self):
        """Resolve only the approved relay service and exclude build/dependency effects."""
        path = trusted(self.active) if self.active.exists() else self.compose_file
        config = json.loads(self.compose(path, "config", "--format", "json", "--no-env-resolution"))
        service = config["services"][self.args.service]
        require(service.get("network_mode") == "host" and not service.get("privileged"),
                "unsafe relay service configuration")
        require(service.get("user") == "10001:10001", "relay must run as 10001:10001")
        require(not any(service.get(key) for key in ("devices", "pid", "ipc", "volumes_from", "secrets", "configs", "env_file")),
                "unsupported relay service attachments")
        mounts = [{"Type": item["type"], "Destination": item["target"],
                   "Source": item.get("source", ""), "RW": not item.get("read_only", False)}
                  for item in service.get("volumes", [])]
        self.validate_mounts(mounts)
        require(service.get("healthcheck", {}).get("test") and not service["healthcheck"].get("disable"),
                "relay healthcheck is required")
        for key in ("build", "depends_on", "profiles", "pull_policy"):
            service.pop(key, None)
        return {"services": {self.args.service: service}}

    def preflight(self):
        """Check deployment and disk capacity without UUID, firewall or container mutations."""
        self.current()
        self.configuration()
        require(self.data.is_dir() and not self.data.is_symlink(), "relay data bind unavailable")
        size = 0
        for directory, _, names in os.walk(self.data, followlinks=False):
            for name in names:
                info = (Path(directory) / name).lstat()
                require(stat.S_ISREG(info.st_mode) or stat.S_ISLNK(info.st_mode),
                        "unsupported special file in relay data")
                size += info.st_size
        require(shutil.disk_usage(self.root).free >= size * 2 + 512 * 1024 * 1024,
                "insufficient updater snapshot/recovery disk space")

    def release(self):
        """Verify immutable local release and prevent downgrade before activation."""
        require(VERSION.fullmatch(self.args.version), "stable release version required")
        require(re.fullmatch(r"ghcr\.io/[a-z0-9_./-]+@sha256:[0-9a-f]{64}", self.args.image),
                "immutable GHCR image required")
        image = json.loads(command(DOCKER + ["image", "inspect", self.args.image]))[0]
        labels = image["Config"].get("Labels") or {}
        require(image["Os"] == "linux" and image["Architecture"] == "amd64"
                and labels.get("io.db-relay.runtime") == CONTRACT
                and labels.get("org.opencontainers.image.version") == self.args.version
                and self.args.image in image.get("RepoDigests", []), "release image contract rejected")
        require(release_newer(self.args.version, self.current()),
                "release must be newer than running version")

    def up(self, path):
        """Recreate only relay from a local image; wait for its mandatory Docker healthcheck."""
        self.compose(path, "up", "-d", "--no-deps", "--no-build", "--pull", "never",
                     "--force-recreate", self.args.service)
        for _ in range(45):
            container = self.container()
            state = container["State"].get("Health", {}).get("Status")
            if state == "healthy":
                return
            require(state != "unhealthy", "relay healthcheck failed")
            time.sleep(1)
        raise RuntimeError("relay healthcheck timed out")

    def stop(self, allow_missing=False):
        """Stop only the validated relay ID; recovery may recreate an absent relay."""
        if allow_missing:
            identifiers = command(DOCKER + ["ps", "-aq", "--filter",
                                  "label=com.docker.compose.project=" + self.args.project,
                                  "--filter", "label=com.docker.compose.service=" + self.args.service]).split()
            if not identifiers:
                return
        container = self.container(running=False)
        command(DOCKER + ["stop", "--time", "20", container["Id"]])

    def apply(self):
        """Persist rollback identity, stop relay, snapshot data, then activate local release."""
        self.preflight()
        self.release()
        require(not self.transaction.exists(), "retained transaction requires finalization")
        container = self.container()
        config = self.configuration()
        self.transaction.mkdir(mode=0o700)
        sync_directory(self.root)
        rollback_tag = "local/db-relay-rollback:" + str(time.time_ns())
        command(DOCKER + ["tag", container["Image"], rollback_tag])
        config["services"][self.args.service]["image"] = rollback_tag
        durable(self.transaction / "previous.json", json.dumps(config).encode())
        durable(self.transaction / "image-id", container["Image"].encode())
        try:
            self.stop()
            command(["/usr/bin/cp", "-a", "--reflink=auto", "--", str(self.data),
                     str(self.transaction / "data")])
            command(["/usr/bin/sync", "-f", str(self.transaction)])
            durable(self.transaction / "backup-ready", b"yes")
            config["services"][self.args.service]["image"] = self.args.image
            durable(self.transaction / "next.json", json.dumps(config).encode())
            self.up(self.transaction / "next.json")
            require(self.current() == self.args.version, "activated version mismatch")
            durable(self.active, (self.transaction / "next.json").read_bytes())
            durable(self.transaction / "applied", b"yes")
        except (RuntimeError, OSError, subprocess.SubprocessError, ValueError, KeyError):
            self.rollback()
            raise RuntimeError("activation failed; rollback completed") from None

    def rollback(self):
        """Restore exact retained image and consistent data; preserve displaced new data."""
        trusted(self.transaction, True)
        config = read_json(self.transaction / "previous.json")
        require(list(config["services"]) == [self.args.service], "invalid rollback service")
        image = config["services"][self.args.service]["image"]
        identity = json.loads(command(DOCKER + ["image", "inspect", image]))[0]
        require(identity["Id"] == (self.transaction / "image-id").read_text(), "rollback image changed")
        if (self.transaction / "rolled-back").exists():
            self.up(self.transaction / "previous.json")
            return
        self.validate_mounts([{"Type": item["type"], "Destination": item["target"],
                              "Source": item.get("source", ""), "RW": not item.get("read_only", False)}
                             for item in config["services"][self.args.service].get("volumes", [])])
        self.stop(allow_missing=True)
        if (self.transaction / "backup-ready").exists():
            # Renames preserve ownership and permit recovery after a kill between data moves.
            restored = self.transaction / "restore-data"
            displaced = self.transaction / "displaced-data"
            if not displaced.exists():
                if restored.exists():
                    shutil.rmtree(restored)
                command(["/usr/bin/cp", "-a", "--reflink=auto", "--",
                         str(self.transaction / "data"), str(restored)])
                command(["/usr/bin/sync", "-f", str(self.transaction)])
                os.rename(self.data, displaced)
            if not self.data.exists():
                os.rename(restored, self.data)
                sync_directory(self.data.parent)
        self.up(self.transaction / "previous.json")
        durable(self.active, json.dumps(config).encode())
        durable(self.transaction / "rolled-back", b"yes")
        applied = self.transaction / "applied"
        if applied.exists():
            applied.unlink()
            sync_directory(self.transaction)

    def finalize(self):
        """Archive a completed transaction without pruning images, backups or Redis."""
        trusted(self.transaction, True)
        require(any((self.transaction / name).is_file() for name in ("applied", "rolled-back")),
                "unfinished transaction cannot be finalized")
        os.rename(self.transaction, self.root / ("archive-" + str(time.time_ns())))
        sync_directory(self.root)


def main():
    """Parse host-only arguments, serialize effects and emit only safe summaries."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=["current", "preflight", "check", "apply", "rollback", "finalize"])
    for name in ("project-dir", "compose-file", "project", "service", "initial-version"):
        parser.add_argument("--" + name, required=True)
    parser.add_argument("--image", default="")
    parser.add_argument("--version", default="")
    args = parser.parse_args()
    adapter = Adapter(args)
    if args.mode in ("current", "preflight", "check"):
        if args.mode == "current":
            print(adapter.current())
        else:
            adapter.preflight()
            if args.mode == "check":
                adapter.release()
        return
    descriptor = os.open(adapter.root / "transaction.lock", os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    try:
        fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        getattr(adapter, args.mode)()
    finally:
        os.close(descriptor)


if __name__ == "__main__":
    try:
        main()
    except (RuntimeError, OSError, subprocess.SubprocessError, ValueError, KeyError):
        # Do not print Docker/config exceptions: their payloads may include credentials.
        print("Ubuntu updater operation failed; retained transaction requires inspection", file=sys.stderr)
        sys.exit(1)
