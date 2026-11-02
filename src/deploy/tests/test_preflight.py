"""Read-only preflight tests using mountinfo fixtures; no host mount or Docker changes."""
from pathlib import Path
import os
import subprocess
import tempfile
import unittest

COMMON = Path(__file__).resolve().parents[1] / 'common.sh'
ROOT = '46 23 8:17 / /mnt/docker-usb rw,noatime - ext4 /dev/sdb1 rw\n'
CHILD = '44 46 8:17 /docker /mnt/docker-usb/docker rw,noatime shared:1 - ext4 /dev/sdb1 rw\n'


class PreflightTests(unittest.TestCase):
    """Check strict self-bind acceptance and the missing-stat fallback."""

    def verify(self, records):
        """Feed mount records to the actual production parser; return exit code."""
        return subprocess.run(['sh', '-c', '. "$1"; docker_mount_record', 'test', str(COMMON)],
                              input=records, text=True, capture_output=True, timeout=5).returncode

    def test_exact_self_bind(self):
        """Record order and propagation flags do not affect exact identity checks."""
        self.assertEqual(self.verify(ROOT + CHILD), 0)
        self.assertEqual(self.verify(CHILD + ROOT), 0)

    def test_foreign_or_ambiguous_mounts_rejected(self):
        """Wrong source/root/parent, readonly and stacked mounts must fail closed."""
        for records in [ROOT, CHILD, ROOT + CHILD + CHILD,
                        ROOT + CHILD.replace('/docker /mnt', '/other /mnt'),
                        ROOT + CHILD.replace('44 46', '44 99'),
                        ROOT + CHILD.replace('8:17', '8:18'),
                        ROOT + CHILD.replace('/dev/sdb1', '/dev/sdc1'),
                        ROOT + CHILD.replace('rw,noatime', 'ro,noatime'),
                        ROOT + CHILD.replace('ext4', 'tmpfs')]:
            with self.subTest(records=records):
                self.assertNotEqual(self.verify(records), 0)

    def test_helper_fallback_without_system_stat(self):
        """An adjacent helper works without PATH tools; missing/symlink helpers fail."""
        with tempfile.TemporaryDirectory() as temporary:
            helper = Path(temporary) / 'deploy-stat'
            helper.write_text('#!/bin/sh\n[ "$1" = -c ] && [ "$2" = %a ] || exit 1\nprintf "700\\n"\n')
            helper.chmod(0o700)
            environment = dict(os.environ, PATH=temporary, HERE=temporary)
            command = ['/bin/sh', '-c', '. "$1"; file_stat -c %a /unused', 'test', str(COMMON)]
            result = subprocess.run(command, env=environment, capture_output=True, text=True, timeout=5)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(result.stdout, '700\n')
            helper.unlink()
            result = subprocess.run(command, env=environment, capture_output=True, timeout=5)
            self.assertNotEqual(result.returncode, 0)
            helper.symlink_to('/bin/sh')
            result = subprocess.run(command, env=environment, capture_output=True, timeout=5)
            self.assertNotEqual(result.returncode, 0)
