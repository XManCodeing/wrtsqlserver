"""Verify the release, installer and systemd units agree on the firewall contract."""
import configparser
import importlib.util
import ipaddress
from pathlib import Path
import shlex
import subprocess
import unittest
from unittest.mock import patch

DEPLOY = Path(__file__).resolve().parents[1]
NAME = "db-relay-firewall"


def unit_settings(suffix, section):
    """Return the selected section from one shipped systemd unit."""
    settings = configparser.ConfigParser(interpolation=None)
    with (DEPLOY / f"{NAME}.{suffix}").open() as stream:
        settings.read_file(stream)
    return settings[section]


class FirewallDeploymentTests(unittest.TestCase):
    """Check the installable package without touching systemd, Docker or UFW."""

    def test_helper_invoked_in_isolation_with_root_owned_config(self):
        """The Ubuntu unit must use a fixed helper; shell expansion is not allowed."""
        service = unit_settings("service", "Service")
        self.assertEqual(service["EnvironmentFile"], "/etc/db-relay-firewall.conf")
        self.assertEqual(shlex.split(service["ExecStart"]), [
            "/usr/bin/python3", "-I", "-B", "/usr/local/lib/db-relay/ubuntu-firewall.py",
            "--project-dir", "${RELAY_PROJECT_DIR}", "--lan-ip", "${RELAY_LAN_IP}",
            "--lan-cidr", "${RELAY_LAN_CIDR}",
        ])

    def test_ubuntu_ss_pads_the_process_column(self):
        """The real Ubuntu iproute2 output pads shorter process descriptions on the right."""
        spec = importlib.util.spec_from_file_location("ufw_smoke", DEPLOY / "ubuntu-firewall.py")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        address = ipaddress.IPv4Address("192.168.10.10")
        network = ipaddress.IPv4Network("192.168.10.0/24")
        row = 'LISTEN 0 4096 0.0.0.0:33063 0.0.0.0:* users:(("db-relay",pid=123,fd=12))      \n'
        with patch.object(module, "container", return_value=("id", 123, "time", 18080)), \
                patch.object(module, "command", side_effect=[row, ""]):
            self.assertEqual(module.live_rules(Path("/srv/test"), {("0.0.0.0", 33063): {network}}, address),
                             {(address, 33063, network)})

    def test_both_triggers_activate_same_service(self):
        """Snapshot changes schedule prompt work; a timer covers missed changes."""
        watcher = unit_settings("path", "Path")
        timer = unit_settings("timer", "Timer")
        self.assertEqual(watcher["PathChanged"], "/www/wwwroot/db-relay/data/relay/firewall-listeners.json")
        self.assertEqual(watcher["Unit"], NAME + ".service")
        self.assertEqual(timer["Unit"], watcher["Unit"])
        self.assertIn("OnBootSec", timer)
        self.assertIn("OnUnitInactiveSec", timer)

    def test_installer_and_release_files(self):
        """All root-side pieces must be included only in the Ubuntu artifact."""
        installer = DEPLOY / "install-ubuntu-firewall.sh"
        subprocess.run(["sh", "-n", installer], check=True)
        workflow = (DEPLOY.parents[1] / ".github/workflows/publish.yml").read_text()
        ubuntu = next(line for line in workflow.splitlines() if '"release/db-relay-ubuntu-$VERSION.tar.gz" -C' in line)
        openwrt = next(line for line in workflow.splitlines() if '"release/db-relay-deploy-$VERSION.tar.gz" -C' in line)
        for item in (installer.name, "ubuntu-firewall.py", NAME + ".service", NAME + ".timer", NAME + ".path"):
            with self.subTest(file=item):
                self.assertIn(item, shlex.split(ubuntu))
                self.assertNotIn(item, shlex.split(openwrt))
                self.assertTrue((DEPLOY / item).is_file())
