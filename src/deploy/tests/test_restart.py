"""Restart-policy and local procd boot-gate regressions; all effects are temporary mocks."""
import json
import subprocess
import unittest
import test_deploy


class RestartTests(unittest.TestCase):
    """Reuse deployment mocks without duplicating the existing regression cases."""

    setUp = test_deploy.DeploymentTests.setUp
    executable = test_deploy.DeploymentTests.executable
    invoke = test_deploy.DeploymentTests.invoke
    commands = test_deploy.DeploymentTests.commands
    relay_fixture = test_deploy.DeploymentTests.relay_fixture
    updater_fixture = test_deploy.DeploymentTests.updater_fixture

    def test_restart_roles_update_rollback_and_repeated_migration(self):
        """Active alone becomes auto-start; retained standby never receives that policy."""
        self.relay_fixture()
        for mode in ('restart-migrate', 'restart-migrate', 'apply'):
            result = self.invoke('relay-update.sh', mode)
            self.assertEqual(result.returncode, 0, result.stderr)
        state = json.loads(self.state.read_text())
        self.assertEqual(state['db-relay']['restart'], 'unless-stopped')
        self.assertEqual(state['db-relay-rollback']['restart'], 'no')
        self.assertTrue(all(call[call.index('--restart')+1] == 'no'
                            for call in self.commands() if call[0] == 'run'))
        for mode in ('restart-migrate', 'rollback', 'rollback', 'restart-migrate'):
            result = self.invoke('relay-update.sh', mode)
            self.assertEqual(result.returncode, 0, result.stderr)
        state = json.loads(self.state.read_text())
        self.assertEqual(state['db-relay']['restart'], 'unless-stopped')
        self.assertEqual(state['db-relay-failed']['restart'], 'no')
        calls = self.commands()
        first_stop = next(index for index, call in enumerate(calls) if call[0] == 'stop')
        self.assertEqual(calls[first_stop-2][0], 'update')

    def test_migration_preserves_manual_stop_and_refuses_unfinished(self):
        """Migration never starts manually stopped containers or repairs ambiguous journals."""
        relay = self.relay_fixture()
        state = json.loads(self.state.read_text())
        state['db-relay']['running'] = False
        self.state.write_text(json.dumps(state))
        result = self.invoke('relay-update.sh', 'restart-migrate')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(json.loads(self.state.read_text())['db-relay']['running'])
        self.assertFalse(any(call[0] == 'start' for call in self.commands()))
        (relay/'transaction').mkdir(mode=0o700)
        result = self.invoke('relay-update.sh', 'restart-migrate')
        self.assertNotEqual(result.returncode, 0)

    def test_interrupted_commit_can_explicitly_complete_policy(self):
        """Power loss after applied marker but before promotion is safe and repairable."""
        self.relay_fixture()
        self.assertEqual(self.invoke('relay-update.sh', 'apply').returncode, 0)
        state = json.loads(self.state.read_text())
        state['db-relay']['restart'] = 'no'
        state['db-relay']['running'] = False
        self.state.write_text(json.dumps(state))
        result = self.invoke('relay-update.sh', 'restart-migrate')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(self.state.read_text())['db-relay']['restart'], 'unless-stopped')
        self.assertFalse(json.loads(self.state.read_text())['db-relay']['running'])

    def test_disable_failure_prevents_stop_or_rename(self):
        """Failure to disable restart leaves original identity/layout unchanged."""
        self.relay_fixture()
        result = self.invoke('relay-update.sh', 'apply', FAIL_UPDATE_POLICY='no')
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(any(call[0] in ('stop', 'rename', 'start', 'run') for call in self.commands()))

    def test_invalid_layout_and_live_lock_refuse_policy_change(self):
        """Restart mutations share the same strict layout and deployment lock as updates."""
        relay = self.relay_fixture()
        state = json.loads(self.state.read_text())
        state['db-relay']['user'] = '0:0 no'
        self.state.write_text(json.dumps(state))
        self.assertNotEqual(self.invoke('relay-update.sh', 'restart-migrate').returncode, 0)
        self.assertFalse(any(call[0] == 'update' for call in self.commands()))
        state['db-relay'].pop('user')
        self.state.write_text(json.dumps(state))
        lock = relay/'.deploy-lock'
        lock.mkdir(mode=0o700)
        (lock/'pid').write_text(str(__import__('os').getpid()))
        (lock/'pid').chmod(0o600)
        self.assertNotEqual(self.invoke('relay-update.sh', 'restart-migrate').returncode, 0)
        self.assertFalse(any(call[0] == 'update' for call in self.commands()))

    def redis_fixture(self):
        """Create isolated Redis layout matching the managed initialization contract."""
        redis = self.usb/'redis-dev'
        redis.mkdir(mode=0o700)
        (redis/'data').mkdir(mode=0o700)
        (redis/'config').mkdir(mode=0o755)
        (redis/'config/redis.conf').write_text('bind 0.0.0.0\n')
        state = {'redis-dev': {
            'id': 'c'*64, 'running': False, 'image': 'sha256:candidate',
            'user': '999:999', 'restart': 'no', 'label': 'redis-v1',
            'ports': '192.168.10.1:6379:6379/tcp;',
            'mount_records': f'{redis}/data|/data|true\n{redis}/config/redis.conf|/usr/local/etc/redis/redis.conf|false',
            'security': 'false|true|default|500000000|268435456|268435456|64|["ALL"]|["no-new-privileges:true"]|[]||null',
        }}
        self.state.write_text(json.dumps(state))
        return state

    def test_redis_migration_preserves_stop_and_validates_layout(self):
        """Redis migration is repeatable and rejects foreign mounts without starts or rule edits."""
        state = self.redis_fixture()
        for _ in range(2):
            result = self.invoke('redis-init.sh', 'restart-migrate')
            self.assertEqual(result.returncode, 0, result.stderr)
        actual = json.loads(self.state.read_text())['redis-dev']
        self.assertEqual(actual['restart'], 'unless-stopped')
        self.assertFalse(actual['running'])
        state['redis-dev']['mount_records'] = '/foreign|/data|true'
        self.state.write_text(json.dumps(state))
        before = len([call for call in self.commands() if call[0] == 'update'])
        self.assertNotEqual(self.invoke('redis-init.sh', 'restart-migrate').returncode, 0)
        self.assertEqual(len([call for call in self.commands() if call[0] == 'update']), before)
        self.assertFalse(any(call[0] in ('start', 'stop', 'run') for call in self.commands()))

    def test_redis_mount_snapshot_accepts_docker_newline_and_order(self):
        """Docker's extra final newline and mount permutations preserve exact validation."""
        state = self.redis_fixture()
        records = state['redis-dev']['mount_records'].splitlines()
        for ordered in (records, list(reversed(records))):
            state['redis-dev']['mount_records'] = '\n'.join(ordered) + '\n'
            self.state.write_text(json.dumps(state))
            result = self.invoke('redis-init.sh', 'restart-migrate')
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(json.loads(self.state.read_text())['redis-dev']['restart'],
                             'unless-stopped')
        # A blank internal record must not be silently filtered out.
        state['redis-dev']['mount_records'] = '\n\n'.join(records) + '\n'
        self.state.write_text(json.dumps(state))
        result = self.invoke('redis-init.sh', 'restart-migrate')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('unsupported Redis mounts', result.stderr)

    def service_fixture(self):
        """Replace only installer trust/platform check and init destination; return init path."""
        import shutil
        target = self.usb/'deploy-new'
        shutil.move(str(self.deploy), target)
        self.deploy = target
        self.updater_fixture()
        # Keep retry loops fast, but allow real child scheduling in bounded CLI probes.
        self.executable('sleep', '#!/bin/sh\n[ "$1" != 1 ] || /bin/sleep 0.05\n')
        script = self.deploy/'updater-service.sh'
        text = script.read_text().replace('INIT=/etc/init.d/relay-updater', 'INIT=$MOCK_ROOT/mock-init')
        text = text.replace('\nservice_check\ncase', '\nservice_check() { :; }\nservice_check\ncase')
        script.write_text(text)
        result = self.invoke('updater-service.sh', 'install')
        self.assertEqual(result.returncode, 0, result.stderr)
        return self.usb/'mock-init'

    def test_procd_upgrade_is_explicit_idempotent_and_rejects_edits(self):
        """Known v1 can upgrade without enable/start; edited init cannot be overwritten."""
        init = self.service_fixture()
        current = init.read_text()
        script = self.deploy/'updater-service.sh'
        prefix = script.read_text().split('\nservice_check\ncase')[0]
        # Emit v1 via the same compatibility renderer, in an isolated sourced script.
        renderer = self.deploy/'render-test.sh'
        renderer.write_text(prefix + '\nrender_legacy\n')
        result = self.invoke('render-test.sh')
        self.assertEqual(result.returncode, 0, result.stderr)
        init.write_text(result.stdout)
        for _ in range(2):
            result = self.invoke('updater-service.sh', 'upgrade')
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(init.read_text(), current)
        init.write_text(current + '\n# foreign customization\n')
        self.assertNotEqual(self.invoke('updater-service.sh', 'upgrade').returncode, 0)

    def test_cross_bundle_upgrade_known_versions_and_rejections(self):
        """Validate full v1/v2/current templates from a distinct trusted USB bundle, never execute."""
        init = self.service_fixture()
        current = init.read_text()
        old = self.usb/'deploy-1.2.0'
        old.mkdir(mode=0o755)
        prefix = (self.deploy/'updater-service.sh').read_text().split('\nservice_check\ncase')[0]
        renderer = self.deploy/'render-test.sh'
        for template in ('render_legacy', 'render_v2', 'render_v3', 'render_service'):
            renderer.write_text(prefix + f'\n{template} "{old}"\n')
            result = self.invoke('render-test.sh')
            self.assertEqual(result.returncode, 0, result.stderr)
            previous = result.stdout
            init.write_text(previous)
            self.assertNotEqual(self.invoke('updater-service.sh', 'upgrade').returncode, 0)
            self.assertEqual(init.read_text(), previous)
            for _ in range(2):
                result = self.invoke('updater-service.sh', 'upgrade', PREVIOUS_BUNDLE=str(old))
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(init.read_text(), current)
            for suffix in ('\n', '\nexit 0\n', '\n# edited\n', '\x00'):
                init.write_text(previous + suffix)
                result = self.invoke('updater-service.sh', 'upgrade', PREVIOUS_BUNDLE=str(old))
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(init.read_text(), previous + suffix)
        init.write_text(previous)
        link = self.usb/'old-link'
        link.symlink_to(old)
        for unsafe in (str(link), str(old)+'/../deploy-1.2.0', str(old)+'/',
                       str(old)+'/./child', str(self.root), str(old)+'//child'):
            self.assertNotEqual(self.invoke('updater-service.sh', 'upgrade', PREVIOUS_BUNDLE=unsafe).returncode, 0)
            self.assertEqual(init.read_text(), previous)
        old.chmod(0o777)
        self.assertNotEqual(self.invoke('updater-service.sh', 'upgrade', PREVIOUS_BUNDLE=str(old)).returncode, 0)

    def test_active_relay_gate_waits_without_starting_or_inspecting_standby(self):
        """Delayed Docker restore retries inspect only; manual stops timeout without running updater."""
        init = self.service_fixture()
        text = init.read_text().replace('if [ "${1:-}" = boot-run ];',
                                       'storage_ready() { return 0; }\nif [ "${1:-}" = boot-run ];')
        init.write_text(text)
        sentinel = self.root/'loaded'
        (self.deploy/'updater-service.sh').write_text(f'#!/bin/sh\ntouch "{sentinel}"\n')
        calls = self.root/'boot-calls'
        self.executable('timeout', '#!/bin/sh\nshift\nexec "$@"\n')
        self.executable('docker', f'''#!/bin/sh
[ "$3" = inspect ] && [ "$6" = db-relay ] || exit 99
printf '%s\\n' "$*" >> '{calls}'
count=$(wc -l < '{calls}')
if [ "$count" -lt 3 ]; then
    printf '/db-relay|relay-v1|false|starting\\n'
else
    printf '/db-relay|relay-v1|true|healthy\\n'
fi
''')
        result = subprocess.run(['sh', str(init), 'boot-run'], env=self.env,
                                text=True, capture_output=True, timeout=15)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(sentinel.exists())
        self.assertEqual(len(calls.read_text().splitlines()), 3)
        sentinel.unlink()
        for state in ('/db-relay|relay-v1|false|healthy', '/db-relay|foreign|true|healthy',
                      '/db-relay|relay-v1|true|starting', '/db-relay|relay-v1|true|unhealthy'):
            self.executable('docker', f'#!/bin/sh\nprintf "%s\\n" "{state}"\n')
            result = subprocess.run(['sh', str(init), 'boot-run'], env=self.env,
                                    text=True, capture_output=True, timeout=15)
            self.assertNotEqual(result.returncode, 0)
            self.assertFalse(sentinel.exists())
            self.assertIn('readiness timeout', result.stderr)

    def test_local_boot_gate_wait_is_bounded_without_loading_usb(self):
        """Unavailable prerequisites never load USB scripts, write USB, or call Docker start."""
        init = self.service_fixture()
        sentinel = self.root/'loaded'
        (self.deploy/'updater-service.sh').write_text(f'#!/bin/sh\ntouch "{sentinel}"\n')
        result = subprocess.run(['sh', str(init), 'boot-run'], env=self.env,
                                text=True, capture_output=True, timeout=15)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('readiness timeout', result.stderr)
        self.assertEqual(len(result.stderr.splitlines()), 2)
        self.assertFalse(sentinel.exists())
        self.assertEqual(self.commands(), [])
        content = init.read_text()
        self.assertIn('bounded_docker --host', content)
        self.assertNotIn('timeout 5 docker', content)
        self.assertNotIn('respawn', content)
        self.assertNotIn("updater-service.sh' check", content)

    def test_bounded_probe_needs_no_timeout_and_reaps_hung_child(self):
        """Generated init preserves output/status and kills a stuck direct Docker child."""
        import os
        import time
        init = self.service_fixture()
        # Isolate the generated helper; forbid accidental use of a system timeout utility.
        self.executable('timeout', '#!/bin/sh\nexit 99\n')
        self.executable('sleep', '#!/bin/sh\nexec /bin/sleep "$@"\n')
        helper = init.read_text().split('if [ "${1:-}" = boot-run ];')[0]
        init.write_text(helper + '\nbounded_docker --host unix:///var/run/docker.sock info\n')
        for status in (0, 7):
            self.executable('docker', f'#!/bin/sh\nprintf "probe-output\\n"\nexit {status}\n')
            result = subprocess.run(['sh', str(init)], env=self.env, text=True,
                                    capture_output=True, timeout=8)
            self.assertEqual(result.returncode, status, result.stderr)
            self.assertEqual(result.stdout, 'probe-output\n')
        child_pid = self.root/'probe-pid'
        self.executable('docker', f'#!/bin/sh\nprintf "%s" "$$" > "{child_pid}"\ntrap "" TERM\nexec /bin/sleep 60\n')
        started = time.monotonic()
        result = subprocess.run(['sh', str(init)], env=self.env, text=True,
                                capture_output=True, timeout=9)
        self.assertEqual(result.returncode, 124, result.stderr)
        self.assertLess(time.monotonic() - started, 8)
        self.assertIn('probe timed out', result.stderr)
        with self.assertRaises(ProcessLookupError):
            os.kill(int(child_pid.read_text()), 0)

    def test_boot_readiness_rejects_wrong_storage_and_nested_bundle(self):
        """Exercise real gate parsing with isolated mount/device/daemon metadata fixtures."""
        init = self.service_fixture()
        bundle = self.usb/'bundle'
        bundle.mkdir(mode=0o755)
        for name in ('updater-service.sh', 'relay-updater', 'deploy-stat'):
            (bundle/name).write_text('#!/bin/sh\nexit 0\n')
        device = self.root/'device'
        device.touch()
        mounts = self.root/'mounts'
        mountinfo = self.root/'mountinfo'
        mountinfo.write_text('')
        mounts.write_text(f'{device} {self.usb} ext4 rw,relatime 0 0\n')
        text = init.read_text().replace('/mnt/docker-usb', str(self.usb))
        text = text.replace(f"BUNDLE='{self.deploy}'", f"BUNDLE='{bundle}'")
        text = text.replace('/proc/mounts', str(mounts)).replace('/proc/self/mountinfo', str(mountinfo))
        text = text.replace('[ -b "$device" ]', '[ -f "$device" ]')
        text = text.replace('if [ "${1:-}" = boot-run ]; then boot_run; exit $?; fi', 'boot_ready')
        init.write_text(text)
        self.executable('ls', '#!/bin/sh\nprintf "drwxr-xr-x 1 0 0 1 Nov 2 00:00 checked\\n"\n')
        self.executable('mountpoint', '#!/bin/sh\nexit 0\n')
        self.executable('readlink', '#!/bin/sh\nprintf "%s\\n" "$2"\n')
        self.executable('timeout', '#!/bin/sh\nshift\nexec "$@"\n')
        self.executable('docker', '#!/bin/sh\ncase "$3" in info) printf "%s/docker\\n" "$MOCK_ROOT";; inspect) printf "/db-relay|relay-v1|true|healthy\\n";; *) exit 99;; esac\n')
        correct_block = '#!/bin/sh\nprintf \'UUID="d7f45318-dc5b-49db-8250-defb536790c6"\\n\'\n'
        self.executable('block', correct_block)

        def probe():
            """Run local generated readiness logic only; return exit code without USB execution."""
            return subprocess.run(['sh', str(init)], env=self.env, capture_output=True, timeout=10).returncode

        self.assertEqual(probe(), 0)
        self.executable('block', '#!/bin/sh\nprintf \'UUID="foreign"\\n\'\n')
        self.assertNotEqual(probe(), 0)
        self.executable('block', correct_block)
        mounts.write_text(f'{device} {self.usb} ext4 ro,relatime 0 0\n')
        self.assertNotEqual(probe(), 0)
        mounts.write_text(f'{device} {self.usb} ext4 rw,relatime 0 0\n')
        mountinfo.write_text(f'10 1 8:2 / {bundle} rw - ext4 /dev/foreign rw\n')
        self.assertNotEqual(probe(), 0)
        mountinfo.write_text('')
        self.executable('docker', '#!/bin/sh\nprintf /wrong/root\n')
        self.assertNotEqual(probe(), 0)

    def test_local_boot_gate_executes_only_after_ready(self):
        """A successful local readiness probe execs the existing runner once."""
        init = self.service_fixture()
        text = init.read_text().replace('if [ "${1:-}" = boot-run ];',
                                       'boot_ready() { return 0; }\nif [ "${1:-}" = boot-run ];')
        init.write_text(text)
        sentinel = self.root/'loaded'
        (self.deploy/'updater-service.sh').write_text(f'#!/bin/sh\nprintf "%s" "$1" > "{sentinel}"\n')
        result = subprocess.run(['sh', str(init), 'boot-run'], env=self.env,
                                text=True, capture_output=True, timeout=15)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(sentinel.read_text(), 'run')

    def test_retained_port_transaction_restores_lan_and_restart_roles(self):
        """Completed port journals restore rules after boot without auto-starting containers."""
        from test_ports import PortDeploymentTests
        relay, _ = PortDeploymentTests.port_fixture(self)
        result = self.invoke('relay-update.sh', 'ports-apply', RELAY_PORTS='3306,13306')
        self.assertEqual(result.returncode, 0, result.stderr)
        state = json.loads(self.state.read_text())
        self.assertEqual(state['db-relay']['restart'], 'unless-stopped')
        self.assertEqual(state['db-relay-rollback']['restart'], 'no')
        self.rules.write_text(json.dumps(['-A PREROUTING -j foreign-proxy']))
        result = self.invoke('lan-manage.sh', 'restore')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(any('--dport 3306 ' in rule for rule in json.loads(self.rules.read_text())))
        (relay/'transaction/applied').unlink()
        before = self.rules.read_text()
        self.assertNotEqual(self.invoke('lan-manage.sh', 'restore').returncode, 0)
        self.assertEqual(self.rules.read_text(), before)

    def test_completed_journal_lan_restore_does_not_block_updater(self):
        """Run tries LAN restore even with retained transaction; failure remains visible."""
        self.service_fixture()
        relay = self.relay_fixture()
        (relay/'transaction').mkdir(mode=0o700)
        (self.usb/'relay-updater/lan-owned').write_text('lan-v1')
        sentinel = self.root/'restored'
        (self.deploy/'lan-restore.sh').write_text(f'#!/bin/sh\ntouch "{sentinel}"\nexit 1\n')
        result = self.invoke('updater-service.sh', 'run')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(sentinel.exists())
        self.assertIn('LAN restoration deferred', result.stderr)


if __name__ == '__main__':
    unittest.main()
