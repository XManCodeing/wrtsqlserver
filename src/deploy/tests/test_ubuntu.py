"""Ubuntu updater policy/transaction tests; mocked Docker, no daemon or root needed."""
import argparse
import importlib.util
import json
from pathlib import Path
import shutil
import tempfile
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("ubuntu_update", Path(__file__).parents[1] / "ubuntu-update.py")
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


INSTALL_SPEC = importlib.util.spec_from_file_location("ubuntu_install", Path(__file__).parents[1] / "ubuntu-install.py")
INSTALL = importlib.util.module_from_spec(INSTALL_SPEC)
INSTALL_SPEC.loader.exec_module(INSTALL)


class UbuntuAdapterTests(unittest.TestCase):
    """Verify explicit Compose scope, contracts and durable rollback behavior."""

    def setUp(self):
        """Create isolated private state and an adapter without root-only constructor."""
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.host = MODULE.Adapter.__new__(MODULE.Adapter)
        self.host.project = Path(self.temporary.name)
        self.host.root = self.host.project / "updater"
        self.host.root.mkdir()
        self.host.transaction = self.host.root / "transaction"
        self.host.data = self.host.project / "data/relay"
        self.host.data.mkdir(parents=True)
        self.host.active = self.host.root / "active-compose.json"
        self.host.compose_file = self.host.project / "compose.ubuntu.yaml"
        self.host.args = argparse.Namespace(service="db-relay", project="db-relay", initial_version="1.2.3",
                                             version="1.2.4", image="ghcr.io/owner/relay@sha256:" + "a" * 64)
        self.host.base = MODULE.DOCKER + ["compose", "--project-directory", str(self.host.project),
                                         "--env-file", str(self.host.project / "data/ubuntu.vars"), "-p", "db-relay"]

    def mounts(self):
        """Return the approved bind fixture including readonly password/socket."""
        return [{"Type": "bind", "Destination": target, "Source": str(source), "RW": writable}
                for target, source, writable in [("/data", self.host.data, True),
                ("/run/secrets/admin_password", self.host.project / "data/secrets/admin_password", False),
                ("/run/relay-updater", self.host.root / "run", False)]]

    def config(self):
        """Return representative Compose config with Redis to ensure projection removes it."""
        volumes = [{"type": item["Type"], "source": item["Source"], "target": item["Destination"],
                    "read_only": not item["RW"]} for item in self.mounts()]
        return {"services": {"db-relay": {"image": "local/relay:ubuntu", "user": "10001:10001",
                "network_mode": "host", "volumes": volumes, "healthcheck": {"test": ["CMD", "true"]},
                "build": {"context": "."}, "depends_on": {"redis-dev": {}}}, "redis-dev": {"image": "redis:7"}}}

    def test_mount_boundary(self):
        """Reject Docker socket, writable secret and Redis data mount substitutions."""
        self.host.validate_mounts(self.mounts())
        for source in ("/var/run/docker.sock", str(self.host.project / "data/redis")):
            mounts = self.mounts()
            mounts[0]["Source"] = source
            with self.assertRaises(RuntimeError):
                self.host.validate_mounts(mounts)
        mounts = self.mounts()
        mounts[1]["RW"] = True
        with self.assertRaises(RuntimeError):
            self.host.validate_mounts(mounts)

    def test_container_rejects_identity_and_version_mismatch(self):
        """Docker filter results must still carry the exact project/service and image version."""
        labels = {"com.docker.compose.project": "db-relay", "com.docker.compose.service": "db-relay",
                  "com.docker.compose.project.working_dir": str(self.host.project),
                  "io.db-relay.runtime": "portable-v1", "org.opencontainers.image.version": "1.2.3-ubuntu"}
        container = {"Config": {"Labels": labels}, "HostConfig": {"NetworkMode": "host"},
                     "Image": "sha256:previous", "State": {"Running": True}, "Mounts": self.mounts()}
        image = {"Config": {"Labels": dict(labels)}}
        def run(arguments):
            """Return a single inspected container and its independently inspected image."""
            if arguments[3] == "ps":
                return "a" * 64
            return json.dumps([image if arguments[3] == "image" else container])
        with patch.object(MODULE, "command", side_effect=run):
            self.host.container()
            labels["com.docker.compose.service"] = "other"
            with self.assertRaisesRegex(RuntimeError, "deployment contract"):
                self.host.container()
            labels["com.docker.compose.service"] = "db-relay"
            image["Config"]["Labels"]["org.opencontainers.image.version"] = "9.0.0"
            with self.assertRaisesRegex(RuntimeError, "version labels differ"):
                self.host.container()

    def test_compose_projection_and_scope(self):
        """Select only relay and forbid dependency/build/pull effects in activation commands."""
        with patch.object(self.host, "compose", return_value=json.dumps(self.config())):
            config = self.host.configuration()
        self.assertEqual(list(config["services"]), ["db-relay"])
        self.assertNotIn("build", config["services"]["db-relay"])
        self.assertNotIn("depends_on", config["services"]["db-relay"])
        with patch.object(MODULE, "command", return_value="") as command, patch.object(
                self.host, "container", return_value={"State": {"Health": {"Status": "healthy"}}}):
            self.host.up(self.host.compose_file)
        arguments = command.call_args.args[0]
        self.assertEqual(arguments[-1], "db-relay")
        self.assertIn("--no-deps", arguments)
        self.assertIn("--no-build", arguments)
        self.assertEqual(arguments[arguments.index("--pull") + 1], "never")
        self.assertNotIn("down", arguments)

    def test_release_rejects_legacy_and_downgrade(self):
        """Matching digests cannot bypass portable contract or strictly-newer version policy."""
        image = {"Os": "linux", "Architecture": "amd64", "RepoDigests": [self.host.args.image],
                 "Config": {"Labels": {"org.opencontainers.image.version": "1.2.4"}}}
        with patch.object(MODULE, "command", side_effect=lambda _: json.dumps([image])), patch.object(
                self.host, "current", return_value="1.2.3"):
            with self.assertRaises(RuntimeError):
                self.host.release()
            image["Config"]["Labels"]["io.db-relay.runtime"] = "portable-v1"
            self.host.release()
        with patch.object(MODULE, "command", return_value=json.dumps([image])), patch.object(
                self.host, "current", return_value="1.2.5"):
            with self.assertRaises(RuntimeError):
                self.host.release()

    def test_rollback_preserves_displaced_data(self):
        """Restore consistent backup and retain new data without touching Redis."""
        self.host.transaction.mkdir()
        backup = self.host.transaction / "data"
        backup.mkdir()
        (backup / "config.json").write_text("old")
        (self.host.data / "config.json").write_text("new")
        (self.host.transaction / "backup-ready").write_text("yes")
        (self.host.transaction / "image-id").write_text("sha256:previous")
        config = self.config()
        del config["services"]["redis-dev"]
        MODULE.durable(self.host.transaction / "previous.json", json.dumps(config).encode())
        def run(arguments):
            """Mock Linux commands on macOS while exercising actual data-directory renames."""
            if arguments[0] == "/usr/bin/docker":
                return json.dumps([{"Id": "sha256:previous"}])
            if arguments[0] == "/usr/bin/cp":
                self.assertIn("-a", arguments)
                shutil.copytree(arguments[-2], arguments[-1])
            else:
                self.assertEqual(arguments[0:2], ["/usr/bin/sync", "-f"])
            return ""

        with patch.object(MODULE, "trusted", side_effect=lambda path, directory=False: path), patch.object(
                MODULE, "command", side_effect=run), patch.object(self.host, "stop") as stop, patch.object(self.host, "up"):
            self.host.rollback()
        stop.assert_called_once_with(allow_missing=True)
        self.assertEqual((self.host.data / "config.json").read_text(), "old")
        self.assertEqual((self.host.transaction / "displaced-data/config.json").read_text(), "new")
        self.assertEqual((self.host.transaction / "rolled-back").read_text(), "yes")

    def test_current_source_build_fallback(self):
        """Preserve exact bootstrap identity even with legacy stable initialVersion config."""
        container = {"Config": {"Labels": {"org.opencontainers.image.version": "1.2.3-ubuntu"}}}
        with patch.object(self.host, "container", return_value=container):
            self.assertEqual(self.host.current(), "1.2.3-ubuntu")
            container["Config"]["Labels"]["org.opencontainers.image.version"] = "9.0.0-dev"
            with self.assertRaises(RuntimeError):
                self.host.current()

    def test_bootstrap_release_promotion_policy(self):
        """Match Go policy: promote exact bootstrap, block downgrades/unknown suffixes/overflow."""
        scenarios = [("1.2.3", "1.2.3-ubuntu", True), ("1.2.4", "1.2.3-ubuntu", True),
                     ("1.2.2", "1.2.3-ubuntu", False), ("1.2.3", "1.2.3", False)]
        for candidate, current, expected in scenarios:
            self.assertEqual(MODULE.release_newer(candidate, current), expected)
        for current in ("1.2.3-rc1", "1.2.2-ubuntu", "1.2.3-ubuntu.1", "1.2.3-ubuntu+build",
                        "v1.2.3-ubuntu", "1.2.18446744073709551616"):
            with self.assertRaises(RuntimeError):
                MODULE.release_newer("1.2.3", current)
        with self.assertRaises(RuntimeError):
            MODULE.release_newer("1.2.3-ubuntu", "1.2.2")
        image = {"Os": "linux", "Architecture": "amd64", "RepoDigests": [self.host.args.image],
                 "Config": {"Labels": {"io.db-relay.runtime": "portable-v1",
                                        "org.opencontainers.image.version": "1.2.3"}}}
        self.host.args.version = "1.2.3"
        with patch.object(MODULE, "command", side_effect=lambda _: json.dumps([image])), patch.object(
                self.host, "current", return_value="1.2.3-ubuntu"):
            self.host.release()
            self.host.args.version = "1.2.2"
            image["Config"]["Labels"]["org.opencontainers.image.version"] = "1.2.2"
            with self.assertRaisesRegex(RuntimeError, "newer"):
                self.host.release()

    def test_apply_failure_keeps_backup_and_rolls_back(self):
        """Activation failure invokes recovery only after durable old-image/data metadata."""
        (self.host.data / "config.json").write_text("original")
        config = self.config()
        del config["services"]["redis-dev"]
        container = {"Image": "sha256:previous"}

        def run(arguments):
            """Emulate only snapshot copy; Docker calls are recorded without a daemon."""
            if arguments[0] == "/usr/bin/cp":
                shutil.copytree(arguments[-2], arguments[-1])
            return ""

        with patch.object(self.host, "preflight"), patch.object(self.host, "release"), patch.object(
                self.host, "container", return_value=container), patch.object(self.host, "configuration", return_value=config), patch.object(
                self.host, "stop"), patch.object(MODULE, "command", side_effect=run), patch.object(
                self.host, "up", side_effect=RuntimeError("health")), patch.object(self.host, "rollback") as rollback:
            with self.assertRaises(RuntimeError):
                self.host.apply()
        rollback.assert_called_once_with()
        self.assertEqual((self.host.transaction / "backup-ready").read_text(), "yes")
        self.assertEqual((self.host.transaction / "data/config.json").read_text(), "original")
        self.assertFalse((self.host.transaction / "applied").exists())

    def test_snapshot_failure_restarts_original_service(self):
        """Snapshot I/O failure after stopping relay must enter recovery before returning."""
        config = self.config()
        del config["services"]["redis-dev"]

        def run(arguments):
            """Fail the snapshot while leaving image-tag commands successful."""
            if arguments[0] == "/usr/bin/cp":
                raise RuntimeError("snapshot failed")
            return ""

        with patch.object(self.host, "preflight"), patch.object(self.host, "release"), patch.object(
                self.host, "container", return_value={"Image": "sha256:previous"}), patch.object(
                self.host, "configuration", return_value=config), patch.object(self.host, "stop"), patch.object(
                MODULE, "command", side_effect=run), patch.object(self.host, "rollback") as rollback:
            with self.assertRaises(RuntimeError):
                self.host.apply()
        rollback.assert_called_once_with()
        self.assertFalse((self.host.transaction / "backup-ready").exists())

    def test_durable_failure_removes_temporary_file(self):
        """Failed atomic replacement leaves old bytes and permits a subsequent recovery write."""
        target = self.host.root / "atomic"
        target.write_bytes(b"original")
        with patch.object(MODULE.os, "replace", side_effect=OSError("disk failure")):
            with self.assertRaises(OSError):
                MODULE.durable(target, b"new")
        self.assertEqual(target.read_bytes(), b"original")
        self.assertEqual(list(self.host.root.glob("atomic.tmp-*")), [])
        MODULE.durable(target, b"recovered")
        self.assertEqual(target.read_bytes(), b"recovered")

    def test_unfinished_transaction_not_finalized(self):
        """Never discard interrupted recovery evidence."""
        self.host.transaction.mkdir()
        with patch.object(MODULE, "trusted", side_effect=lambda path, directory=False: path):
            with self.assertRaises(RuntimeError):
                self.host.finalize()
        self.assertTrue(self.host.transaction.exists())


class UbuntuEnableTests(unittest.TestCase):
    """Exercise original-image enablement and recovery without Linux/root/Docker."""

    mounts = UbuntuAdapterTests.mounts
    config = UbuntuAdapterTests.config

    def setUp(self):
        """Extend relay fixture with original private variables and enable transaction."""
        UbuntuAdapterTests.setUp(self)
        self.host.variables = self.host.project / "data/ubuntu.vars"
        self.original = b"# preserve LAN\nLAN_BIND_IP=192.168.1.20\nRELAY_IMAGE=sha256:previous\nONLINE_UPDATE_SOCKET=\n"
        self.host.variables.write_bytes(self.original)
        self.host.transaction = self.host.root / "enable-transaction"
        self.host.args.binary = "/root/build/relay-updater"
        self.host.args.relay_image = ""
        self.host.args.initial_version = "1.2.3-ubuntu"
        self.original_data = '{"mappings":[{"listenPort":3306}]}'
        (self.host.data / "config.json").write_text(self.original_data)

    def mock_command(self, arguments):
        """Emulate image identity and Linux archival copies, preserving data fixture."""
        if arguments[:4] == MODULE.DOCKER + ["image"]:
            return json.dumps([{"Id": "sha256:previous"}])
        if arguments[0] == "/usr/bin/cp":
            shutil.copytree(arguments[-2], arguments[-1])
        return ""

    def activation_context(self):
        """Return reusable mocks for preflight and fixed source image/config."""
        from contextlib import ExitStack
        context = ExitStack()
        context.enter_context(patch.object(self.host, "preflight"))
        context.enter_context(patch.object(self.host, "current", return_value=self.host.args.initial_version))
        context.enter_context(patch.object(self.host, "container", return_value={"Image": "sha256:previous"}))
        config = self.config()
        del config["services"]["redis-dev"]
        config["services"]["db-relay"]["environment"] = {"ONLINE_UPDATE_SOCKET": "", "REDIS_ADDR": "192.168.1.20:6379"}
        context.enter_context(patch.object(self.host, "configuration", return_value=config))
        context.enter_context(patch.object(self.host, "stop"))
        context.enter_context(patch.object(INSTALL.UPDATE, "command", side_effect=self.mock_command))
        context.enter_context(patch.object(INSTALL, "install"))
        self.addCleanup(context.close)
        return context

    def test_socket_variable_edit_preserves_other_settings(self):
        """Change only disabled socket line, leaving image/LAN/comments untouched."""
        enabled = INSTALL.socket_variables(self.original)
        self.assertEqual(enabled.replace(INSTALL.SOCKET.encode(), b""), self.original)
        for invalid in (b"ONLINE_UPDATE_SOCKET=/unexpected\n", b"", b"ONLINE_UPDATE_SOCKET=\nONLINE_UPDATE_SOCKET=\n"):
            with self.assertRaises(RuntimeError):
                INSTALL.socket_variables(invalid)

    def test_enable_preserves_image_data_and_redis(self):
        """Enable on old image with retained 3306 configuration and relay-only projection."""
        with self.activation_context(), patch.object(self.host, "up") as up:
            INSTALL.activate(self.host.args, self.host)
        activated = json.loads((self.host.transaction / "next.json").read_text())
        self.assertEqual(list(activated["services"]), ["db-relay"])
        relay = activated["services"]["db-relay"]
        self.assertEqual(relay["image"], "sha256:previous")
        self.assertEqual(relay["environment"]["ONLINE_UPDATE_SOCKET"], INSTALL.SOCKET)
        self.assertEqual(relay["environment"]["REDIS_ADDR"], "192.168.1.20:6379")
        self.assertEqual((self.host.data / "config.json").read_text(), self.original_data)
        self.assertEqual((self.host.transaction / "data/config.json").read_text(), self.original_data)
        up.assert_called_once_with(self.host.transaction / "next.json")

    def test_local_image_contract_and_safe_version(self):
        """Require both runtime contracts, Linux amd64 and same/upgrade-only version identity."""
        image = {"Id": "sha256:" + "b" * 64, "Os": "linux", "Architecture": "amd64",
                 "Config": {"Labels": {"io.db-relay.runtime": "portable-v1", "io.db-relay.network.host": "v1",
                                        "org.opencontainers.image.version": "1.2.3-ubuntu"}}}
        with patch.object(INSTALL.UPDATE, "command", side_effect=lambda _: json.dumps([image])):
            for version in ("1.2.3-ubuntu", "1.2.3", "1.2.4"):
                image["Config"]["Labels"]["org.opencontainers.image.version"] = version
                self.assertEqual(INSTALL.target_image("local/db-relay:ubuntu-online", "1.2.3-ubuntu"), (image["Id"], version))
            for version in ("1.2.2", "1.2.3-custom", "1.2.4-ubuntu"):
                image["Config"]["Labels"]["org.opencontainers.image.version"] = version
                with self.assertRaises(RuntimeError):
                    INSTALL.target_image("local/db-relay:ubuntu-online", "1.2.3-ubuntu")
            image["Config"]["Labels"]["org.opencontainers.image.version"] = "1.2.3-ubuntu"
            for field, invalid in (("Os", "windows"), ("Architecture", "arm64")):
                original = image[field]
                image[field] = invalid
                with self.assertRaisesRegex(RuntimeError, "contract"):
                    INSTALL.target_image("local/db-relay:ubuntu-online", "1.2.3-ubuntu")
                image[field] = original
            for label in ("io.db-relay.runtime", "io.db-relay.network.host"):
                original = image["Config"]["Labels"].pop(label)
                with self.assertRaisesRegex(RuntimeError, "contract"):
                    INSTALL.target_image("local/db-relay:ubuntu-online", "1.2.3-ubuntu")
                image["Config"]["Labels"][label] = original

    def test_local_image_enable_pins_id_and_retains_original(self):
        """Use new image ID in vars/Compose while retaining original bytes/image/data for rollback."""
        self.host.args.relay_image = "local/db-relay:ubuntu-online"
        new_id = "sha256:" + "b" * 64
        with self.activation_context(), patch.object(INSTALL, "target_image", return_value=(new_id, "1.2.3-ubuntu")), patch.object(
                self.host, "container", side_effect=[{"Image": "sha256:previous"}, {"Image": new_id}]), patch.object(self.host, "up"):
            INSTALL.activate(self.host.args, self.host)
        next_config = json.loads((self.host.transaction / "next.json").read_text())
        self.assertEqual(next_config["services"]["db-relay"]["image"], new_id)
        self.assertIn(("RELAY_IMAGE=" + new_id).encode(), self.host.variables.read_bytes())
        self.assertEqual((self.host.transaction / "ubuntu.vars").read_bytes(), self.original)
        self.assertEqual((self.host.transaction / "image-id").read_text(), "sha256:previous")
        self.assertEqual((self.host.data / "config.json").read_text(), self.original_data)

    def test_local_image_upgrade_checks_expected_version(self):
        """Approved bootstrap-to-stable replacement verifies candidate identity rather than old version."""
        self.host.args.relay_image = "local/db-relay:ubuntu-online"
        new_id = "sha256:" + "b" * 64
        with self.activation_context(), patch.object(INSTALL, "target_image", return_value=(new_id, "1.2.3")), patch.object(
                self.host, "current", side_effect=["1.2.3-ubuntu", "1.2.3"]), patch.object(
                self.host, "container", side_effect=[{"Image": "sha256:previous"}, {"Image": new_id}]), patch.object(self.host, "up"):
            INSTALL.activate(self.host.args, self.host)
        self.assertTrue((self.host.transaction / "applied").exists())

    def test_rejected_local_image_never_installs_or_stops(self):
        """Invalid candidate is rejected before updater installation or any relay mutation."""
        self.host.args.relay_image = "local/db-relay:ubuntu-online"
        with self.activation_context(), patch.object(INSTALL, "target_image", side_effect=RuntimeError("contract")), patch.object(
                INSTALL, "install") as install, patch.object(self.host, "stop") as stop:
            with self.assertRaisesRegex(RuntimeError, "contract"):
                INSTALL.activate(self.host.args, self.host)
        install.assert_not_called()
        stop.assert_not_called()
        self.assertEqual(self.host.variables.read_bytes(), self.original)

    def test_local_image_activation_failure_restores_old_pin(self):
        """Failed new-image health check restores old RELAY_IMAGE and socket together."""
        self.host.args.relay_image = "local/db-relay:ubuntu-online"
        with self.activation_context(), patch.object(INSTALL, "target_image", return_value=("sha256:" + "b" * 64, "1.2.3-ubuntu")), patch.object(
                self.host, "up", side_effect=RuntimeError("health")), patch.object(INSTALL.UPDATE, "trusted", side_effect=lambda path, directory=False: path), patch.object(
                self.host, "rollback") as rollback:
            with self.assertRaisesRegex(RuntimeError, "original relay restored"):
                INSTALL.activate(self.host.args, self.host)
        rollback.assert_called_once_with()
        self.assertEqual(self.host.variables.read_bytes(), self.original)
        self.assertEqual((self.host.transaction / "data/config.json").read_text(), self.original_data)

    def test_installer_accepts_exact_bootstrap_initial_version(self):
        """Root-approved bootstrap config retains exact identity and unknown suffixes fail closed."""
        args = argparse.Namespace(**vars(self.host.args))
        args.project_dir = str(self.host.project)
        args.compose_file = str(self.host.compose_file)
        self.host.variables.chmod(0o600)
        with patch.object(INSTALL.os, "geteuid", return_value=0), patch.object(INSTALL, "trusted", side_effect=Path):
            self.assertEqual(INSTALL.validate(args)[0], self.host.project)
            args.initial_version = "1.2.4-ubuntu"
            with self.assertRaises(RuntimeError):
                INSTALL.validate(args)

    def test_initial_identity_mismatch_rejected_before_install(self):
        """Never normalize bootstrap to stable during initial enable identity validation."""
        with self.activation_context(), patch.object(self.host, "current", return_value="1.2.3"), patch.object(INSTALL, "install") as install:
            with self.assertRaisesRegex(RuntimeError, "initial version"):
                INSTALL.activate(self.host.args, self.host)
        install.assert_not_called()

    def test_enable_failure_restores_original_variables(self):
        """Health failure restores exact vars and invokes durable image/data recovery."""
        with self.activation_context(), patch.object(self.host, "up", side_effect=RuntimeError("health")), patch.object(
                INSTALL.UPDATE, "trusted", side_effect=lambda path, directory=False: path), patch.object(self.host, "rollback") as rollback:
            with self.assertRaisesRegex(RuntimeError, "original relay restored"):
                INSTALL.activate(self.host.args, self.host)
        self.assertEqual(self.host.variables.read_bytes(), self.original)
        rollback.assert_called_once_with()
        self.assertFalse(self.host.active.exists())

    def test_installer_failure_never_stops_relay(self):
        """Binary/systemd installation failure occurs before container or variable writes."""
        with self.activation_context(), patch.object(INSTALL, "install", side_effect=RuntimeError("systemd")), patch.object(self.host, "stop") as stop:
            with self.assertRaises(RuntimeError):
                INSTALL.activate(self.host.args, self.host)
        stop.assert_not_called()
        self.assertEqual(self.host.variables.read_bytes(), self.original)
        self.assertFalse(self.host.transaction.exists())

    def test_enable_snapshot_failure_restores_vars_without_data_backup(self):
        """Snapshot failure after stop invokes recovery and never marks partial backup ready."""
        def fail_copy(arguments):
            """Fail only data copy, allowing immutable image resolution and tagging."""
            if arguments[0] == "/usr/bin/cp":
                raise RuntimeError("snapshot failed")
            return self.mock_command(arguments)
        with self.activation_context(), patch.object(INSTALL.UPDATE, "command", side_effect=fail_copy), patch.object(
                INSTALL.UPDATE, "trusted", side_effect=lambda path, directory=False: path), patch.object(self.host, "rollback") as rollback:
            with self.assertRaisesRegex(RuntimeError, "original relay restored"):
                INSTALL.activate(self.host.args, self.host)
        rollback.assert_called_once_with()
        self.assertFalse((self.host.transaction / "backup-ready").exists())
        self.assertEqual(self.host.variables.read_bytes(), self.original)

    def test_recovery_failure_keeps_snapshot(self):
        """Failed rollback is actionable and retains original vars/data for explicit recovery."""
        with self.activation_context(), patch.object(self.host, "up", side_effect=RuntimeError("health")), patch.object(
                INSTALL.UPDATE, "trusted", side_effect=lambda path, directory=False: path), patch.object(self.host, "rollback", side_effect=RuntimeError("recovery")):
            with self.assertRaisesRegex(RuntimeError, "--recover-enable"):
                INSTALL.activate(self.host.args, self.host)
        self.assertTrue((self.host.transaction / "backup-ready").exists())
        self.assertEqual(self.host.variables.read_bytes(), self.original)

    def test_stale_enable_recovery_rejected(self):
        """Initial snapshot cannot override a later completed online update."""
        self.host.transaction.mkdir()
        (self.host.root / "archive-later").mkdir()
        with patch.object(INSTALL.UPDATE, "trusted", side_effect=lambda path, directory=False: path):
            with self.assertRaisesRegex(RuntimeError, "no longer safe"):
                INSTALL.recover(self.host)

    def test_install_start_failure_removes_only_new_files(self):
        """Failed systemd start cleans new install while retaining run directory and old data."""
        binary = self.host.project / "relay-updater"
        binary.write_bytes(b"\x7fELF\x02\x01" + b"\x00" * 12 + b"\x3e\x00")
        args = argparse.Namespace(**vars(self.host.args))
        args.binary = str(binary)
        args.compose_file = str(self.host.compose_file)
        unit = self.host.project / "db-relay-updater.service"
        def directory(path, mode=0o700, group=0):
            """Create mocked Linux directories without chown on the local test host."""
            path.mkdir(mode=mode, exist_ok=True)
        def systemctl(*arguments):
            """Fail service activation but permit deterministic cleanup calls."""
            if arguments[0] == "enable":
                raise RuntimeError("service failed")
        with patch.object(INSTALL, "validate", return_value=(self.host.project, self.host.root)), patch.object(
                INSTALL.UPDATE, "trusted", side_effect=lambda path, directory=False: Path(path)), patch.object(
                INSTALL, "UNIT", unit), patch.object(INSTALL, "directory", side_effect=directory), patch.object(
                INSTALL, "systemctl", side_effect=systemctl):
            with self.assertRaisesRegex(RuntimeError, "service failed"):
                INSTALL.install(args)
        self.assertFalse(unit.exists())
        self.assertFalse((self.host.root / "config.json").exists())
        self.assertFalse((self.host.root / "bin/relay-updater").exists())
        self.assertTrue((self.host.root / "run").is_dir())
        self.assertEqual((self.host.data / "config.json").read_text(), self.original_data)

    def test_unit_limits_writes_to_project_data(self):
        """Unit has no TCP listener, retains root sandbox and writable relay transaction path."""
        unit = INSTALL.service_unit(self.host.project, self.host.root)
        self.assertIn("ProtectSystem=strict", unit)
        self.assertIn(f"ReadWritePaths={self.host.project}/data", unit)
        self.assertIn("User=root", unit)
        self.assertNotIn("ListenStream", unit)


if __name__ == "__main__":
    unittest.main()
