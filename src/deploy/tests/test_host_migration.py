"""Contract tests for Ubuntu migration helpers, without Docker or root privileges."""
from pathlib import Path
import subprocess
import unittest


class HostMigrationTests(unittest.TestCase):
    """Preserve existing data and keep firewall changes outside automatic migration."""

    def test_shell_syntax_and_boundaries(self):
        """Check syntax and required recovery guards; no remote operations are performed."""
        deploy = Path(__file__).resolve().parents[1]
        for name in ("host-network-check.sh", "host-network-migrate.sh"):
            result = subprocess.run(["sh", "-n", str(deploy / name)], capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
        source = (deploy / "host-network-migrate.sh").read_text()
        self.assertIn("mode=${3:-check}", source)
        self.assertIn('sh "$HERE/host-network-check.sh"', source)
        self.assertIn("--no-build --pull never --wait", source)
        self.assertIn('old_compose stop db-relay', source)
        self.assertIn('tar -czf "$backup/relay-data.tar.gz"', source)
        self.assertIn('old_compose up -d', source)
        self.assertNotIn("down -v", source)
        self.assertNotIn("ufw allow", source)
        self.assertNotIn("ufw disable", source)
        self.assertNotIn("docker.sock:/", source)
        self.assertNotIn("rm -rf", source)

    def test_invalid_arguments_do_not_operate(self):
        """Missing arguments fail before any host probing, filesystem writes or Docker calls."""
        deploy = Path(__file__).resolve().parents[1]
        for name in ("host-network-check.sh", "host-network-migrate.sh"):
            result = subprocess.run(["sh", str(deploy / name)], capture_output=True, text=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("usage:", result.stderr)


if __name__ == "__main__":
    unittest.main()
