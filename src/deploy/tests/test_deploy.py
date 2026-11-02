"""Isolated shell integration tests: mock every Docker call; never contact Docker.

Run: python3 -m unittest discover -s src/deploy/tests -v
All service files and fake executable state live in TemporaryDirectory. Production
scripts are copied and the USB preflight alone is replaced with a temp-root guard;
separate tests exercise the real LAN and symlink rejection helpers.
"""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

DEPLOY = Path(__file__).resolve().parents[1]
MOCK_DOCKER = r'''#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
state_file = Path(os.environ['MOCK_STATE'])
state = json.loads(state_file.read_text())
args = sys.argv[1:]
assert args[:2] == ['--host', 'unix:///var/run/docker.sock'], args
args = args[2:]
with open(os.environ['MOCK_LOG'], 'a') as log:
    log.write(json.dumps(args) + '\n')
def save():
    state_file.write_text(json.dumps(state))
def output(value):
    print(value)
    sys.exit(0)
if args[:2] == ['image', 'inspect']:
    if os.environ.get('MISSING_IMAGE'): sys.exit(1)
    output(os.environ.get('MOCK_ARCH', 'linux/amd64') + ' ' + os.environ.get('MOCK_IMAGE', 'sha256:candidate'))
if args[:2] == ['container', 'ls']:
    if '--filter' not in args:
        output('')
    if os.environ.get('LIST_FAIL'): sys.exit(1)
    name = args[args.index('--filter')+1][7:-1]
    output(state.get(name, {}).get('id', ''))
if args[0] == 'inspect':
    container = state.get(args[-1]) or next((item for item in state.values() if item['id'] == args[-1]), None)
    if not container: sys.exit(1)
    fmt = args[args.index('--format')+1]
    if fmt == '{{.Id}}': output(container['id'])
    if fmt == '{{.Image}}': output(container.get('image', 'sha256:old'))
    if 'deploy.managed' in fmt: output(container.get('label', 'relay-v1'))
    if 'deploy.transaction' in fmt: output(container.get('transaction', 'initial'))
    if '.Config.Env' in fmt:
        output(container.get('online_env', 'ONLINE_UPDATE_SOCKET=/run/relay-updater/control.sock' if container.get('online') else ''))
    if '.Mounts' in fmt:
        if 'ne .Type' in fmt: output(container.get('extra_mounts', 'tmpfs:/tmp;'))
        if '.RW' in fmt: output(container.get('bind_modes', 'true;false;false;' if container.get('online') else 'true;false;'))
        root = os.environ['MOCK_ROOT'] + '/db-relay'
        mounts = root+'/data:/data;'+root+'/admin_password:/run/secrets/admin_password;'
        if container.get('online'): mounts += os.environ['MOCK_ROOT']+'/relay-updater/run:/run/relay-updater;'
        output(container.get('mounts', mounts))
    if '.HostConfig.PortBindings' in fmt:
        output(container.get('ports', '192.168.10.1:13306:13306/tcp;192.168.10.1:18080:8080/tcp;'))
    if '.HostConfig.Privileged' in fmt:
        output(container.get('security', 'false|true|default|500000000|134217728|134217728|64|["ALL"]|["no-new-privileges:true"]|[]||null'))
    if '.Config.User' in fmt: output(container.get('user', '10001:10001 no'))
    if fmt == '{{.State.Status}}': output(container.get('status', 'running' if container['running'] else 'exited'))
    if fmt == '{{.State.Running}}': output(str(container['running']).lower())
    if '.State.ExitCode' in fmt: output('false 0')
    if '.State.Health.Status' in fmt:
        healthy = not (os.environ.get('FAIL_HEALTH') and container['id'] != 'a'*64)
        output(str(container['running']).lower() + (' healthy' if healthy else ' unhealthy'))
    raise AssertionError(fmt)
if args[0] == 'rm':
    assert len(args) == 2 and len(args[1]) == 64, args
    names = [name for name, item in state.items() if item['id'] == args[1]]
    assert len(names) == 1 and names[0] in ('db-relay-rollback', 'db-relay-failed'), args
    assert state[names[0]]['running'] is False
    del state[names[0]]
    save(); sys.exit(0)
if args[0] in ('stop', 'start'):
    state[args[-1]]['running'] = args[0] == 'start'
    save(); sys.exit(0)
if args[0] == 'rename':
    assert args[2] not in state
    state[args[2]] = state.pop(args[1]); save(); sys.exit(0)
if args[0] == 'run':
    name = args[args.index('--name')+1]
    assert name not in state
    transaction = next((arg.split('=', 1)[1] for arg in args if arg.startswith('io.db-relay.deploy.transaction=')), 'initial')
    state[name] = {'id': os.environ.get('MOCK_ID', 'b'*64), 'running': True, 'transaction': transaction,
                   'image': args[-1],
                   'ports': ''.join(value + ';' for index, value in enumerate(args) if index > 0 and args[index-1] == '-p'),
                   'online': 'ONLINE_UPDATE_SOCKET=/run/relay-updater/control.sock' in args}
    save()
    if name == 'db-relay' and transaction != 'initial':
        data = Path(os.environ['MOCK_ROOT'])/'db-relay'/'data'
        (data/'config.enc').write_text('candidate-config')
    if os.environ.get('FAIL_RUN'): sys.exit(1)
    output('b'*64)
raise AssertionError(args)
'''


class DeploymentTests(unittest.TestCase):
    """Exercise copied scripts with fake executables and private temporary data."""

    def setUp(self):
        """Create isolated fixture roots and executable mocks; returns None."""
        self.temporary = tempfile.TemporaryDirectory(dir='/tmp')
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        self.deploy = self.root / 'deploy'
        shutil.copytree(DEPLOY, self.deploy, ignore=shutil.ignore_patterns('tests'))
        self.usb = self.root / 'usb'
        self.usb.mkdir(mode=0o700)
        common = self.deploy / 'common.sh'
        common.write_text(common.read_text() + '''\n# Test-only replacements, never installed on host.
USB=$MOCK_ROOT
RELAY=$USB/db-relay
REDIS=$USB/redis-dev
usb_check() { [ "$USB" = "$MOCK_ROOT" ] || exit 99; }
private_dir() {
    safe_path "$1"
    [ -d "$1" ] && [ "$(stat -c %a "$1")" = 700 ] || fail 'unsafe test directory'
}
private_file() {
    safe_path "$1"
    [ -f "$1" ] && [ "$(stat -c '%a:%h' "$1")" = 600:1 ] || fail 'unsafe test file'
}
''')
        self.bin = self.root / 'bin'
        self.bin.mkdir()
        self.executable('docker', MOCK_DOCKER)
        self.executable('ip', '#!/bin/sh\nprintf "    inet 192.168.10.1/24 scope global br-lan\\n"\n')
        self.executable('wget', '#!/bin/sh\nprintf \'{"authenticated":false}\\n\'\n')
        self.executable('chown', '#!/bin/sh\nexit 0\n')
        self.executable('sleep', '#!/bin/sh\nexit 0\n')
        self.executable('sync', '#!/bin/sh\nexit 0\n')
        self.executable('mountpoint', '#!/bin/sh\nexit 1\n')
        # macOS stat is BSD; faithfully emulate only the fields these scripts use.
        self.executable('stat', '''#!/usr/bin/env python3
import os, stat, sys
info = os.stat(sys.argv[-1])
values = {'%u': str(info.st_uid), '%g': str(info.st_gid), '%a': oct(stat.S_IMODE(info.st_mode))[2:], '%h': str(info.st_nlink), '%d': str(info.st_dev)}
if os.environ.get('MOCK_TRUST_ROOT'):
    values['%u'] = '0'
    if not sys.argv[-1].startswith(os.environ['MOCK_TRUST_ROOT']): values['%a'] = '755'
if os.environ.get('MOCK_UPDATER_OWNER') and '/relay-updater' in sys.argv[-1]:
    values['%u'] = '0'
    values['%g'] = '10001'
result = sys.argv[2]
for key, value in values.items(): result = result.replace(key, value)
print(result)
''')
        self.state = self.root / 'state.json'
        self.state.write_text('{}')
        self.log = self.root / 'commands.jsonl'
        self.env = dict(os.environ, PATH=f'{self.bin}:{os.environ["PATH"]}',
                        MOCK_ROOT=str(self.usb), MOCK_STATE=str(self.state),
                        MOCK_LOG=str(self.log), LAN_INTERFACE='br-lan',
                        LAN_BIND_IP='192.168.10.1', LAN_INTERFACE_CONFIRMED='yes',
                        REDIS_LAN_ACCESS_CONFIRMED='yes', RELAY_IMAGE='local/db-relay:2.0.0')

    def executable(self, name, content):
        """Write a fake executable by name/content inside the temp bin; returns None."""
        target = self.bin / name
        target.write_text(content)
        target.chmod(0o700)

    def invoke(self, script, mode=None, **environment):
        """Execute only a copied script with mocks; return captured CompletedProcess."""
        args = ['sh', str(self.deploy / script)]
        if mode:
            args.append(mode)
        return subprocess.run(args, env=dict(self.env, **environment), text=True,
                              capture_output=True, timeout=60)

    def commands(self):
        """Return all mock Docker arguments, or empty list when not invoked."""
        return [json.loads(line) for line in self.log.read_text().splitlines()] if self.log.exists() else []

    def relay_fixture(self):
        """Prepare a managed original relay plus consistent private config/key pair."""
        relay = self.usb / 'db-relay'
        relay.mkdir(mode=0o700)
        (relay / 'data').mkdir(mode=0o700)
        for relative, content in [('data/config.enc', 'old-config'), ('data/secret.key', 'old-key'), ('admin_password', 'private-password')]:
            target = relay / relative
            target.write_text(content)
            target.chmod(0o600)
        self.state.write_text(json.dumps({'db-relay': {'id': 'a'*64, 'running': True}}))
        return relay

    def test_redis_default_check_readonly(self):
        """Default Redis invocation must not create directories or mutate Docker."""
        result = self.invoke('redis-init.sh')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(list(self.usb.iterdir()), [])
        self.assertTrue(all(command[0] in ('image', 'container') for command in self.commands()))

    def test_redis_mock_start_security_and_no_overwrite(self):
        """Fake start checks fixed image/resource flags and rejects a second start."""
        result = self.invoke('redis-init.sh', 'start')
        self.assertEqual(result.returncode, 0, result.stderr)
        run = next(command for command in self.commands() if command[0] == 'run')
        for flag in ('--pull', 'never', '--read-only', '--cap-drop', 'ALL', '256m', '999:999'):
            self.assertIn(flag, run)
        self.assertIn('192.168.10.1:6379:6379/tcp', run)
        self.assertEqual(self.invoke('redis-init.sh', 'start').returncode, 1)

    def test_lan_confirmation_and_image_rejections(self):
        """Unconfirmed/public/wildcard/missing/non-amd64 cases fail closed."""
        for environment in ({'REDIS_LAN_ACCESS_CONFIRMED': ''}, {'LAN_INTERFACE_CONFIRMED': ''},
                            {'LAN_BIND_IP': '0.0.0.0'}, {'LAN_BIND_IP': '8.8.8.8'},
                            {'LAN_BIND_IP': '192.168.10.2'}, {'LAN_BIND_IP': '010.0.0.1'},
                            {'MISSING_IMAGE': '1'}, {'MOCK_ARCH': 'linux/arm64'}, {'LIST_FAIL': '1'}):
            with self.subTest(environment=environment):
                self.assertNotEqual(self.invoke('redis-init.sh', **environment).returncode, 0)
        self.assertEqual(list(self.usb.iterdir()), [])

    def test_symlink_rejected(self):
        """Service symlink cannot redirect initialization outside its own directory."""
        (self.usb / 'redis-dev').symlink_to(self.root)
        result = self.invoke('redis-init.sh', 'start')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('symlink', result.stderr)

    def test_update_check_readonly(self):
        """Update default performs only inspect/list and does not create a journal."""
        relay = self.relay_fixture()
        result = self.invoke('relay-update.sh')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse((relay / 'transaction').exists())
        self.assertTrue(all(command[0] in ('image', 'container', 'inspect') for command in self.commands()))

    def test_apply_and_explicit_rollback(self):
        """Successful update retains old container; rollback restores matching pair."""
        relay = self.relay_fixture()
        result = self.invoke('relay-update.sh', 'apply')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('db-relay-rollback', json.loads(self.state.read_text()))
        self.assertEqual((relay/'transaction/backup/config.enc').read_text(), 'old-config')
        result = self.invoke('relay-update.sh', 'rollback')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((relay/'data/config.enc').read_text(), 'old-config')
        self.assertEqual((relay/'data/secret.key').read_text(), 'old-key')
        self.assertEqual((relay/'transaction/failed-data/config.enc').read_text(), 'candidate-config')
        self.assertEqual(json.loads(self.state.read_text())['db-relay']['id'], 'a'*64)
        self.assertIn('REDIS_ADDR=192.168.10.1:6379', next(command for command in self.commands() if command[0] == 'run'))
        self.assertFalse(any('redis-dev' in command for command in self.commands()))
        self.assertFalse(any(command[0] in ('rm', 'rmi', 'prune', 'pull') for command in self.commands()))

    def test_unhealthy_candidate_auto_rollback(self):
        """Health failure automatically returns to the original container/config."""
        relay = self.relay_fixture()
        result = self.invoke('relay-update.sh', 'apply', FAIL_HEALTH='1')
        self.assertNotEqual(result.returncode, 0)
        self.assertTrue((relay/'transaction/rolled-back').exists(), result.stderr)
        self.assertEqual((relay/'data/config.enc').read_text(), 'old-config')
        self.assertEqual(json.loads(self.state.read_text())['db-relay']['id'], 'a'*64)

    def test_run_failure_after_creation_auto_rollback(self):
        """Ambiguous run failure still finds its labelled candidate and rolls back."""
        relay = self.relay_fixture()
        result = self.invoke('relay-update.sh', 'apply', FAIL_RUN='1')
        self.assertNotEqual(result.returncode, 0)
        self.assertTrue((relay/'transaction/rolled-back').exists(), result.stderr)

    def test_unmanaged_container_and_unknown_data_refused(self):
        """Never mutate an unmanaged container or silently omit unknown state files."""
        relay = self.relay_fixture()
        state = json.loads(self.state.read_text())
        state['db-relay']['label'] = 'foreign'
        self.state.write_text(json.dumps(state))
        self.assertNotEqual(self.invoke('relay-update.sh', 'apply').returncode, 0)
        state['db-relay']['label'] = 'relay-v1'
        self.state.write_text(json.dumps(state))
        (relay/'data/unexpected').write_text('data')
        self.assertNotEqual(self.invoke('relay-update.sh', 'apply').returncode, 0)
        self.assertFalse((relay/'transaction').exists())

    def test_interrupted_rename_recovery(self):
        """Simulate crash after old rename and backup commit, before candidate run."""
        relay = self.relay_fixture()
        result = self.invoke('relay-update.sh', 'apply')
        self.assertEqual(result.returncode, 0, result.stderr)
        state = json.loads(self.state.read_text())
        state.pop('db-relay')
        self.state.write_text(json.dumps(state))
        (relay/'transaction/applied').unlink()
        lock = relay/'.deploy-lock'
        lock.mkdir(mode=0o700)
        (lock/'pid').write_text('99999999\n')
        (lock/'pid').chmod(0o600)
        self.assertNotEqual(self.invoke('relay-update.sh', 'rollback').returncode, 0)
        result = self.invoke('relay-update.sh', 'rollback', STALE_LOCK_CONFIRMED='yes')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((relay/'data/config.enc').read_text(), 'old-config')

    def test_live_lock_blocks_rollback(self):
        """Even explicit stale confirmation cannot bypass an active PID lock."""
        relay = self.relay_fixture()
        result = self.invoke('relay-update.sh', 'apply')
        self.assertEqual(result.returncode, 0, result.stderr)
        lock = relay/'.deploy-lock'
        lock.mkdir(mode=0o700)
        (lock/'pid').write_text(str(os.getpid())+'\n')
        (lock/'pid').chmod(0o600)
        result = self.invoke('relay-update.sh', 'rollback', STALE_LOCK_CONFIRMED='yes')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('another deployment is active', result.stderr)

    def test_restore_copy_failure_stops_recovery(self):
        """Failed restore IO must not rename/start original with partial data."""
        relay = self.relay_fixture()
        result = self.invoke('relay-update.sh', 'apply')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.executable('cp', '#!/bin/sh\nexit 19\n')
        result = self.invoke('relay-update.sh', 'rollback')
        self.assertNotEqual(result.returncode, 0)
        state = json.loads(self.state.read_text())
        self.assertIn('db-relay-rollback', state)
        self.assertNotIn('db-relay', state)
        self.assertFalse((relay/'transaction/rolled-back').exists())
        self.assertTrue((relay/'transaction/backup/secret.key').exists())

    def test_finalize_then_second_apply(self):
        """Archive snapshot, remove stopped original by ID and allow another upgrade."""
        relay = self.relay_fixture()
        self.assertEqual(self.invoke('relay-update.sh', 'apply').returncode, 0)
        result = self.invoke('relay-update.sh', 'finalize')
        self.assertEqual(result.returncode, 0, result.stderr)
        history = list((relay/'history').iterdir())
        self.assertEqual(len(history), 1)
        self.assertEqual((history[0]/'backup/secret.key').read_text(), 'old-key')
        self.assertFalse((relay/'transaction').exists())
        self.assertEqual([cmd for cmd in self.commands() if cmd[0] == 'rm'], [['rm', 'a'*64]])
        result = self.invoke('relay-update.sh', 'apply', MOCK_IMAGE='sha256:second', MOCK_ID='c'*64)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((relay/'transaction/original-id').read_text().strip(), 'b'*64)
        self.assertFalse(any('redis-dev' in cmd for cmd in self.commands()))

    def test_rollback_finalize_and_reapply(self):
        """Rolled-back completion retains candidate data and removes only failed ID."""
        relay = self.relay_fixture()
        self.assertEqual(self.invoke('relay-update.sh', 'apply').returncode, 0)
        self.assertEqual(self.invoke('relay-update.sh', 'rollback').returncode, 0)
        result = self.invoke('relay-update.sh', 'finalize')
        self.assertEqual(result.returncode, 0, result.stderr)
        history = next((relay/'history').iterdir())
        self.assertEqual((history/'failed-data/config.enc').read_text(), 'candidate-config')
        self.assertEqual([cmd for cmd in self.commands() if cmd[0] == 'rm'], [['rm', 'b'*64]])
        self.assertEqual(self.invoke('relay-update.sh', 'apply', MOCK_ID='c'*64).returncode, 0)

    def test_finalize_rejects_unsafe_identity_layout_and_running(self):
        """Any identity, ownership, layout or running mismatch prevents removal."""
        self.relay_fixture()
        self.assertEqual(self.invoke('relay-update.sh', 'apply').returncode, 0)
        baseline = json.loads(self.state.read_text())
        changes = [('db-relay-rollback', 'status', 'dead'),
                   ('db-relay-rollback', 'running', True), ('db-relay-rollback', 'id', 'c'*64),
                   ('db-relay-rollback', 'label', 'foreign'), ('db-relay', 'id', 'd'*64),
                   ('db-relay', 'user', '0:0 no'), ('db-relay-rollback', 'ports', 'foreign'),
                   ('db-relay-rollback', 'mounts', 'foreign'), ('db-relay', 'bind_modes', 'true;true;')]
        for name, key, value in changes:
            with self.subTest(name=name, key=key):
                modified = json.loads(json.dumps(baseline))
                modified[name][key] = value
                self.state.write_text(json.dumps(modified))
                self.assertNotEqual(self.invoke('relay-update.sh', 'finalize').returncode, 0)
        self.assertFalse(any(cmd[0] == 'rm' for cmd in self.commands()))

    def test_finalize_unknown_or_interrupted_fails_closed(self):
        """Unexpected state or durable finalizing intent cannot trigger cleanup."""
        relay = self.relay_fixture()
        self.assertEqual(self.invoke('relay-update.sh', 'apply').returncode, 0)
        for marker in ('unknown', 'finalizing'):
            path = relay/'transaction'/marker
            path.write_text('yes\n')
            path.chmod(0o600)
            self.assertNotEqual(self.invoke('relay-update.sh', 'finalize').returncode, 0)
            path.unlink()
        (relay/'transaction/applied').unlink()
        self.assertNotEqual(self.invoke('relay-update.sh', 'finalize').returncode, 0)
        self.assertFalse(any(cmd[0] == 'rm' for cmd in self.commands()))

    def test_online_bind_is_preserved(self):
        """A validated socket enables online control and survives another update."""
        self.relay_fixture()
        home = self.usb/'relay-updater'
        home.mkdir(mode=0o700)
        run = home/'run'
        run.mkdir(mode=0o750)
        # Sandbox disallows Unix bind syscalls: emulate inode type only; keep
        # production ownership, mode, symlink and directory validation intact.
        common = self.deploy/'common.sh'
        common.write_text(common.read_text().replace('[ -S "$USB/relay-updater/run/control.sock" ]', '[ -f "$USB/relay-updater/run/control.sock" ]'))
        (run/'control.sock').write_text('mock socket inode')
        (run/'control.sock').chmod(0o660)
        environment = {'MOCK_UPDATER_OWNER': 'yes'}
        result = self.invoke('relay-update.sh', 'apply', **environment)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(json.loads(self.state.read_text())['db-relay']['online'])
        self.assertEqual(self.invoke('relay-update.sh', 'finalize', **environment).returncode, 0)
        result = self.invoke('relay-update.sh', 'apply', MOCK_IMAGE='sha256:second', MOCK_ID='c'*64, **environment)
        self.assertEqual(result.returncode, 0, result.stderr)
        (run/'control.sock').chmod(0o666)
        self.assertNotEqual(self.invoke('relay-update.sh', 'finalize', **environment).returncode, 0)

    def updater_fixture(self):
        """Prepare mock root-owned startup files and bounded POSIX disk report."""
        home = self.usb/'relay-updater'
        home.mkdir(mode=0o700)
        (home/'run').mkdir(mode=0o750)
        (home/'config.json').write_text('{}')
        (home/'config.json').chmod(0o600)
        binary = self.deploy/'relay-updater'
        binary.write_text('#!/bin/sh\nexit 0\n')
        binary.chmod(0o700)
        self.executable('df', '#!/bin/sh\nprintf "Filesystem 1024-blocks Used Available Capacity Mounted\\n/dev/mock 900000 100000 800000 12%% $MOCK_ROOT\\n"\n')
        self.executable('setsid', '#!/bin/sh\nexec "$@"\n')
        self.env.update(MOCK_UPDATER_OWNER='yes', MOCK_TRUST_ROOT=str(self.root))
        return home

    def test_finalize_health_and_missing_candidate_refused(self):
        """Unhealthy active service or missing saved candidate forbids cleanup."""
        relay = self.relay_fixture()
        self.assertEqual(self.invoke('relay-update.sh', 'apply').returncode, 0)
        self.assertNotEqual(self.invoke('relay-update.sh', 'finalize', FAIL_HEALTH='1').returncode, 0)
        self.assertFalse(any(cmd[0] == 'rm' for cmd in self.commands()))
        self.assertEqual(self.invoke('relay-update.sh', 'rollback').returncode, 0)
        state = json.loads(self.state.read_text())
        del state['db-relay-failed']
        self.state.write_text(json.dumps(state))
        self.assertNotEqual(self.invoke('relay-update.sh', 'finalize').returncode, 0)
        self.assertTrue((relay/'transaction').exists())

    def test_updater_preflight_and_start_default_readonly(self):
        """Preflight needs no image; startup default neither writes nor launches."""
        home = self.updater_fixture()
        result = self.invoke('updater-preflight.sh', MISSING_IMAGE='1')
        self.assertEqual(result.returncode, 0, result.stderr)
        result = self.invoke('updater-start.sh', MISSING_IMAGE='1')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse((home/'.daemon-lock').exists())
        self.assertEqual(self.commands(), [])

    def test_updater_disk_and_path_rejections(self):
        """Insufficient/ambiguous disk output and unsafe runtime permissions fail."""
        home = self.updater_fixture()
        for available in ('524287', 'not-a-number', '9999999999999999'):
            self.executable('df', '#!/bin/sh\nprintf "header\\n/dev/mock 900000 1 '+available+' 1%% $MOCK_ROOT\\n"\n')
            self.assertNotEqual(self.invoke('updater-preflight.sh').returncode, 0)
        self.executable('df', '#!/bin/sh\nprintf "header\\n/dev/mock 900000 1 524288 1%% $MOCK_ROOT\\n"\n')
        result = self.invoke('updater-preflight.sh')
        self.assertEqual(result.returncode, 0, result.stderr)
        (home/'run').chmod(0o777)
        self.assertNotEqual(self.invoke('updater-preflight.sh').returncode, 0)
        (home/'run').chmod(0o750)
        (home/'run').rmdir()
        (home/'run').symlink_to(self.root)
        self.assertNotEqual(self.invoke('updater-preflight.sh').returncode, 0)

    def test_updater_config_and_lock_rejections(self):
        """No unsafe config or existing PID lock can launch another daemon."""
        home = self.updater_fixture()
        (home/'config.json').chmod(0o644)
        self.assertNotEqual(self.invoke('updater-start.sh').returncode, 0)
        (home/'config.json').chmod(0o600)
        (home/'.daemon-lock').mkdir(mode=0o700)
        self.assertNotEqual(self.invoke('updater-start.sh', 'start').returncode, 0)
        self.assertEqual(self.commands(), [])

    def test_shell_syntax(self):
        """Every production shell file must parse with local POSIX sh."""
        for script in DEPLOY.glob('*.sh'):
            result = subprocess.run(['sh', '-n', str(script)], capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)


if __name__ == '__main__':
    unittest.main()
