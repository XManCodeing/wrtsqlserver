"""Validate the portable Compose contract without contacting Docker or a host.

Run with PyYAML installed: python3 -m unittest discover -s src/deploy/tests -p test_portable.py -v
"""
from pathlib import Path
import unittest

try:
    import yaml
except ImportError:
    yaml = None


@unittest.skipIf(yaml is None, "PyYAML is required for Compose syntax validation")
class PortableComposeTests(unittest.TestCase):
    """Keep the panel-managed deployment independent of router-specific services."""

    def test_ubuntu_host_manifest(self):
        """Host listeners retain bind paths and password-free Redis on the explicit LAN IP."""
        source = Path(__file__).resolve().parents[2] / "compose.ubuntu.yaml"
        services = yaml.safe_load(source.read_text())["services"]
        self.assertEqual(set(services), {"db-relay", "redis-dev"})
        relay, redis = services["db-relay"], services["redis-dev"]
        self.assertEqual(relay["network_mode"], "host")
        self.assertNotIn("ports", relay)
        self.assertEqual(relay["environment"]["RELAY_NETWORK_MODE"], "host")
        self.assertEqual(relay["environment"]["REDIS_ADDR"], "${LAN_BIND_IP:?Set Ubuntu LAN IPv4}:6379")
        self.assertIn("18080", relay["environment"]["LISTEN_ADDR"])
        self.assertIn('${LAN_BIND_IP:?', relay["healthcheck"]["test"][1])
        self.assertEqual(redis["ports"], ["${LAN_BIND_IP:?Set Ubuntu LAN IPv4}:6379:6379/tcp"])
        self.assertNotIn("--requirepass", redis["command"])
        self.assertNotIn("--aclfile", redis["command"])
        self.assertEqual(redis["command"][redis["command"].index("--protected-mode") + 1], "no")
        self.assertNotIn("network_mode", redis)
        for service in services.values():
            for mount in service["volumes"]:
                self.assertEqual(mount["type"], "bind")
                self.assertTrue(mount["source"].startswith("./data/"))
                self.assertFalse(mount["bind"]["create_host_path"])
            self.assertNotIn("docker.sock", str(service))
            self.assertFalse(service.get("privileged", False))

    def test_two_isolated_services(self):
        """Parse the manifest and check storage, startup and host privilege boundaries."""
        source = Path(__file__).resolve().parents[2] / "compose.yaml"
        document = yaml.safe_load(source.read_text())
        services = document["services"]
        self.assertEqual(set(services), {"db-relay", "redis-dev"})
        self.assertEqual(set(document["volumes"]), {"relay-data", "redis-data"})
        for service in services.values():
            self.assertEqual(service["restart"], "unless-stopped")
            self.assertFalse(service.get("privileged", False))
            self.assertNotEqual(service.get("network_mode"), "host")
            self.assertNotIn("depends_on", service)
            self.assertIn("healthcheck", service)
            self.assertIn("logging", service)
            encoded = str(service)
            for forbidden in ("docker.sock", "relay-updater", "/mnt/docker-usb", "br-lan", "UUID"):
                self.assertNotIn(forbidden, encoded)
        relay = services["db-relay"]
        self.assertEqual(relay["environment"]["REDIS_ADDR"], "redis-dev:6379")
        self.assertNotIn("ONLINE_UPDATE_SOCKET", relay["environment"])
        self.assertEqual(relay["user"], "10001:10001")
        self.assertTrue(relay["read_only"])
        self.assertTrue(relay["volumes"][1]["read_only"])
        self.assertFalse(relay["volumes"][1]["bind"]["create_host_path"])
        self.assertNotIn("ports", services["redis-dev"])
        self.assertIn("--appendonly", services["redis-dev"]["command"])
        for publication in relay["ports"]:
            self.assertTrue(publication.startswith("${LAN_BIND_IP:?"))


if __name__ == "__main__":
    unittest.main()
