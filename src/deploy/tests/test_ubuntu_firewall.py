"""Isolated Ubuntu firewall policy tests: no host Docker, UFW, /proc, sockets or root."""
from contextlib import nullcontext
import importlib.util
import json
from pathlib import Path
import stat
from types import SimpleNamespace
import unittest
from unittest.mock import MagicMock, Mock, mock_open, patch


SPEC = importlib.util.spec_from_file_location('ubuntu_firewall', Path(__file__).parents[1] / 'ubuntu-firewall.py')
FW = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(FW)
PROJECT = Path('/www/wwwroot/db-relay')
TAG = FW.marker(PROJECT)
ADDRESS, LAN = FW.lan_policy('192.168.10.10', '192.168.10.0/24')
KEY = (ADDRESS, 33063, LAN)
CID = 'd' * 64
IDENTITY = (CID, 123, '2026-01-01T00:00:00.1Z', 18080)
ARGS = ['--project-dir', str(PROJECT), '--lan-ip', str(ADDRESS), '--lan-cidr', str(LAN)]


def snapshot(host='0.0.0.0', port=33063, sources=None):
    """Return v1 snapshot bytes for a single mock relay listener and its source ACL."""
    return json.dumps({'version': 1, 'listeners': [{'host': host, 'port': port,
                      'sources': [str(LAN)] if sources is None else sources}]}).encode()


def row(key=KEY, owner='', **changes):
    """Return a UFW rule object using the actual distro backend's field names/encoding."""
    dst, port, src = key
    data = dict(v6=False, forward=False, direction='in', protocol='tcp', sport='any', dapp='', sapp='',
                interface_in='', interface_out='', dst=str(dst), src=str(src), dport=str(port),
                action='allow', logtype='', comment=owner.encode().hex())
    data.update(changes)
    return SimpleNamespace(**data)


class MemoryFirewall:
    """Record mock host mutations while retaining duplicate and manually owned records."""

    def __init__(self, *rules):
        """Initialize the simulated UFW inventory from typed UFW rule records."""
        self.records = list(rules)
        self.changes = []

    def rules(self):
        """Return the full simulated inventory, including duplicate rules."""
        return self.records

    def mutate(self, key, tag, delete):
        """Apply an exact tagged mock rule addition/deletion and record its effect."""
        self.changes.append((key, delete))
        item = row(key, FW.rule_marker(tag, key))
        if delete:
            self.records.remove(item)
        else:
            self.records.append(item)


class PolicyTests(unittest.TestCase):
    """Exercise untrusted snapshot validation, fixed paths and least-privilege intersection."""

    def test_lan_scope_and_project_paths(self):
        """Reject public, loopback, supernet, invalid path and off-subnet operator inputs."""
        for host, cidr in [('8.8.8.8', '8.8.8.0/24'), ('127.0.0.1', '127.0.0.0/8'),
                           (str(ADDRESS), '0.0.0.0/0'), (str(ADDRESS), '10.0.0.0/8'),
                           ('192.168.10.255', str(LAN)), (str(ADDRESS), '192.168.10.1/24')]:
            with self.subTest(host=host, cidr=cidr), self.assertRaises((ValueError, FW.PolicyError)):
                FW.lan_policy(host, cidr)
        for name in ('/', '/etc', '/tmp/test', '/opt', '/opt/../srv/test', 'relay', '//opt/relay', '/opt//test'):
            with self.subTest(name=name), self.assertRaises(FW.PolicyError):
                FW.project_path(name)
        self.assertEqual(FW.project_path(str(PROJECT)), PROJECT)

    def test_acl_intersection_and_reserved_listeners(self):
        """Clip supersets to LAN, retain /32 ACL, never allow arbitrary hosts or service ports."""
        reader = lambda content: FW.candidates(content, ADDRESS, LAN)
        self.assertEqual(reader(snapshot(sources=['0.0.0.0/0', '10.0.0.0/8'])), {('0.0.0.0', 33063): {LAN}})
        self.assertEqual(reader(snapshot(str(ADDRESS), sources=['8.8.8.0/24', '192.168.10.8/32'])),
                         {(str(ADDRESS), 33063): {FW.V4('192.168.10.8/32')}})
        for host in ('127.0.0.1', '8.8.8.8', '192.168.10.9'):
            self.assertFalse(reader(snapshot(host)))
        for port in (22, 443, 6379, 8080, 18080):
            self.assertFalse(reader(snapshot(port=port)))
        self.assertFalse(reader(snapshot(sources=[])))

    def test_bad_schema_fails_closed(self):
        """Reject duplicate fields, invalid types, oversize arrays, malformed IPs and rule bombs."""
        for content in (b'{', b'{"version":1,"version":1,"listeners":[]}', b'{"version":true,"listeners":[]}',
                        snapshot(port=True), snapshot(port=65536), snapshot(sources=['::/0']),
                        snapshot(sources=['0.0.0.0/0;touch']), snapshot(sources=['10.0.0.0/8'] * 65)):
            with self.subTest(content=content[:70]), self.assertRaises(FW.PolicyError):
                FW.candidates(content, ADDRESS, LAN)
        with patch.object(FW, 'MAX_RULES', 0), self.assertRaises(FW.PolicyError):
            FW.candidates(snapshot(), ADDRESS, LAN)

    def test_pinned_read_rejects_symlinks_devices_and_oversize(self):
        """Mock Linux O_PATH pinning, no file contents opened before metadata validation."""
        regular = dict(st_uid=10001, st_nlink=1, st_mode=stat.S_IFREG | 0o600, st_size=10,
                       st_mtime_ns=1, st_ctime_ns=1)
        for change in (dict(st_mode=stat.S_IFLNK), dict(st_mode=stat.S_IFCHR), dict(st_mode=stat.S_IFIFO),
                       dict(st_nlink=2), dict(st_size=FW.MAX_FILE + 1), dict(st_uid=777)):
            relay = SimpleNamespace(st_uid=10001, st_mode=stat.S_IFDIR | 0o700)
            meta = SimpleNamespace(**{**regular, **change})
            with patch.object(FW.os, 'O_PATH', 0x200000, create=True), patch.object(FW.os, 'open', return_value=3) as opened, \
                 patch.object(FW.os, 'close'), patch.object(FW, 'trusted_dir') as trusted, \
                 patch.object(FW.os, 'fstat', side_effect=[relay, meta]), patch('builtins.open') as reader, \
                 self.assertRaises(FW.PolicyError):
                FW.read_snapshot(PROJECT)
            reader.assert_not_called()
            self.assertEqual(trusted.call_count, len(PROJECT.parts) + 1)
            self.assertEqual(opened.call_args.args, ('firewall-listeners.json', 0x200000 | FW.os.O_NOFOLLOW))
            for item in opened.call_args_list[:-1]:
                self.assertEqual(item.args[1], FW.DIRECTORY)

    def test_pinned_regular_file_and_concurrent_change(self):
        """Read exactly the pinned inode; stop on short read or concurrent file mutation."""
        data = snapshot()
        relay = SimpleNamespace(st_uid=10001, st_mode=stat.S_IFDIR | 0o700)
        info = SimpleNamespace(st_uid=10001, st_mode=stat.S_IFREG | 0o600, st_size=len(data),
                               st_nlink=1, st_mtime_ns=2, st_ctime_ns=3)
        for suffix in (data, data[:-1]):
            mock = mock_open(read_data=suffix)
            with patch.object(FW.os, 'O_PATH', 0x200000, create=True), patch.object(FW.os, 'open', return_value=7), \
                 patch.object(FW.os, 'close'), patch.object(FW, 'trusted_dir'), patch('builtins.open', mock), \
                 patch.object(FW.os, 'fstat', side_effect=[relay, info, info]):
                if suffix == data:
                    self.assertEqual(FW.read_snapshot(PROJECT), data)
                else:
                    with self.assertRaises(FW.PolicyError):
                        FW.read_snapshot(PROJECT)
            mock.assert_called_once_with('/proc/self/fd/7', 'rb')


class OwnershipTests(unittest.TestCase):
    """Verify independent Docker identity checks and exact IPv4 socket owner/endpoint checks."""

    def test_container_projection_and_identity(self):
        """Reject a foreign working directory, process, missing management address or ambiguous IDs."""
        fields = [CID, 'running', '123', IDENTITY[2], 'host', '/app/db-relay', 'db-relay', 'db-relay',
                  str(PROJECT), 'relay-ubuntu-v1', 'bind ' + str(PROJECT / 'data/relay'),
                  'LISTEN_ADDR=192.168.10.10:18080']
        with patch.object(FW, 'command', side_effect=[CID, '\n'.join(fields)]) as run:
            self.assertEqual(FW.container(PROJECT), IDENTITY)
        self.assertIn('--format', run.call_args.args[0])
        for index, value in ((0, 'e'*64), (1, 'paused'), (2, '1'), (4, 'bridge'), (5, 'sshd'),
                             (6, 'other'), (7, 'redis'), (8, '/opt/other'), (9, ''),
                             (10, 'bind /opt/other/data/relay'), (11, '')):
            changed = fields.copy()
            changed[index] = value
            with self.subTest(index=index), patch.object(FW, 'command', side_effect=[CID, '\n'.join(changed)]), \
                    self.assertRaises(FW.PolicyError):
                FW.container(PROJECT)
        with patch.object(FW, 'command', return_value=CID+'\n'+CID), self.assertRaises(FW.PolicyError):
            FW.container(PROJECT)

    def test_ss_family_endpoint_pid_and_exclusivity(self):
        """Require only one owner/socket; v6 wildcard, missing owners and borrowed ports are rejected."""
        ipv4 = 'LISTEN 0 4096 0.0.0.0:33063 0.0.0.0:* users:(("db-relay",pid=123,fd=9))'
        ipv6 = 'LISTEN 0 128 [::]:33063 [::]:* users:(("other",pid=50,fd=3))'
        cases = [(ipv4, '', '0.0.0.0', True), (ipv4.replace('0.0.0.0:33063', '*:33063'), '', '0.0.0.0', True),
                 (ipv4, '', str(ADDRESS), False), (ipv4, ipv6, '0.0.0.0', False),
                 (ipv4 + '\n' + ipv4, '', '0.0.0.0', False), (ipv4.split(' users')[0], '', '0.0.0.0', False),
                 (ipv4.replace('pid=123', 'pid=1234'), '', '0.0.0.0', False),
                 (ipv4.replace('db-relay', 'sshd'), '', '0.0.0.0', False),
                 (ipv4.replace('0.0.0.0:', str(ADDRESS)+':'), '', str(ADDRESS), True)]
        for v4, v6, host, expect in cases:
            with self.subTest(v4=v4, v6=v6), patch.object(FW, 'command', side_effect=[v4, v6]), \
                    patch.object(FW, 'container', return_value=IDENTITY):
                self.assertEqual(FW.live_rules(PROJECT, {(host, 33063): {LAN}}, ADDRESS), {KEY} if expect else set())
        with patch.object(FW, 'container', return_value=IDENTITY[:-1] + (33063,)), \
             patch.object(FW, 'command', side_effect=[ipv4, '']):
            self.assertFalse(FW.live_rules(PROJECT, {('0.0.0.0', 33063): {LAN}}, ADDRESS))

    def test_container_stopped_or_restarted(self):
        """Stopped container yields withdrawals; restarted container invalidates the entire observation."""
        wanted = {('0.0.0.0', 33063): {LAN}}
        with patch.object(FW, 'container', return_value=None), patch.object(FW, 'command') as run:
            self.assertFalse(FW.live_rules(PROJECT, wanted, ADDRESS))
            run.assert_not_called()
        with patch.object(FW, 'container', side_effect=[IDENTITY, None]), patch.object(FW, 'command', return_value=''), \
                self.assertRaises(FW.PolicyError):
            FW.live_rules(PROJECT, wanted, ADDRESS)


class RulesTests(unittest.TestCase):
    """Verify rule ownership, duplicate protection, safe withdrawals and CLI exit behavior."""

    def test_manual_rule_and_spoofed_comments_never_changed(self):
        """Preserve manual equivalent /32 spellings, Unicode/fake display rows and v6 siblings."""
        key = (ADDRESS, 33063, FW.V4('192.168.10.1/32'))
        fake = '# comment\n[ 1] fake ALLOW IN # ' + FW.rule_marker(TAG, key)
        for comment in ('', 'manually managed', fake, FW.rule_marker(TAG, KEY)):
            manual = row(key, comment, dst=str(ADDRESS)+'/32', src='192.168.10.1')
            v6 = row(key, '', v6=True, src='::1', dst='::1')
            firewall = MemoryFirewall(manual, v6)
            FW.synchronize(firewall, {key}, TAG)
            FW.synchronize(firewall, set(), TAG)
            self.assertEqual(firewall.records, [manual, v6])
            self.assertFalse(firewall.changes)

    def test_distinct_owners_and_ambiguity(self):
        """Do not edit an equivalent deny/logged rule, a duplicate or a copied project tag."""
        tagged = row(KEY, FW.rule_marker(TAG, KEY))
        for rows in ([tagged, row()], [tagged, tagged], [row(), row()], [row(action='deny')], [row(logtype='log')]):
            firewall = MemoryFirewall(*rows)
            with self.assertLogs(level='ERROR'), self.assertRaises(FW.PolicyError):
                FW.synchronize(firewall, {KEY}, TAG)
            self.assertFalse(firewall.changes)
        foreign = row(KEY, FW.rule_marker(FW.marker(Path('/opt/other')), KEY))
        firewall = MemoryFirewall(foreign)
        FW.synchronize(firewall, set(), TAG)
        self.assertEqual(firewall.records, [foreign])

    def test_move_remove_first_and_verify_idempotence(self):
        """Replace old scope before adding new scope; a second reconciliation is a no-op."""
        previous = (FW.IP('10.1.2.3'), 3333, FW.V4('10.1.2.0/24'))
        firewall = MemoryFirewall(row(previous, FW.rule_marker(TAG, previous)), row(dport='22'))
        FW.synchronize(firewall, {KEY}, TAG)
        FW.synchronize(firewall, {KEY}, TAG)
        self.assertEqual(firewall.changes, [(previous, True), (KEY, False)])
        FW.synchronize(firewall, set(), TAG)
        self.assertEqual(firewall.records, [row(dport='22')])

    def test_batch_limits_and_cleanup_failures(self):
        """Bound each invocation; log failed removals and attempt remaining safe cleanup."""
        next_key = (ADDRESS, 33064, LAN)
        rules = [row(key, FW.rule_marker(TAG, key)) for key in (KEY, next_key)]
        firewall = MemoryFirewall(*rules)
        with patch.object(FW, 'MAX_MUTATIONS', 1), self.assertRaises(FW.PolicyError):
            FW.synchronize(firewall, set(), TAG)
        self.assertEqual(firewall.changes, [(KEY, True)])
        firewall = MemoryFirewall(*rules)
        with patch.object(firewall, 'mutate', side_effect=OSError()) as mutation, \
             self.assertLogs(level='ERROR') as logs, self.assertRaises(FW.PolicyError):
            FW.synchronize(firewall, set(), TAG)
        self.assertEqual(mutation.call_count, 2)
        self.assertIn('cleanup failed', logs.output[0])

    def test_live_status_and_exact_specification(self):
        """Use distro status header but never parse forged rows, and delete only a full v4 rule."""
        facade = FW.Firewall.__new__(FW.Firewall)
        backend = MagicMock()
        facade.make_frontend = Mock(return_value=SimpleNamespace(backend=backend, do_action=Mock(return_value='Rule deleted')))
        backend.get_status.return_value = 'Status: inactive'
        with self.assertRaises(FW.PolicyError):
            facade.rules()
        facade.make_frontend.return_value.do_action.assert_not_called()
        backend.get_status.return_value = 'Status: active\n\n     To       Action   From\n[ 1] malicious display'
        self.assertIs(facade.rules(), backend.get_rules.return_value)
        tag = FW.rule_marker(TAG, KEY)
        facade.parse = Mock(return_value=SimpleNamespace(action='allow', data={'iptype': 'v4',
                            'rule': row(KEY, tag, remove=True)}))
        facade.mutate(KEY, TAG, True)
        facade.parse.assert_called_once_with(['ufw','delete','allow','in','proto','tcp','from',str(LAN),
                                              'to',str(ADDRESS),'port','33063','comment',tag])

    def test_ufw_diagnostics_and_unapplied_changes(self):
        """Reject warnings, UFW's internal sys.exit and apparent success without persisted change."""
        def skipped():
            """Simulate distro warning for silently skipped tuple; return no data."""
            print('skipped malformed tuple', file=FW.sys.stderr)
        with self.assertRaisesRegex(FW.PolicyError, 'warning'):
            FW.distro_call(skipped)
        with self.assertRaisesRegex(FW.PolicyError, 'rejected'):
            FW.distro_call(Mock(side_effect=SystemExit(1)))
        firewall = MemoryFirewall()
        with patch.object(firewall, 'mutate'), self.assertRaisesRegex(FW.PolicyError, 'postcondition'):
            FW.synchronize(firewall, {KEY}, TAG)

    def test_cli_cleanup_missing_bad_snapshot_and_exit(self):
        """CLI cleanup ignores untrusted data; invalid or missing snapshots withdraw then fail."""
        for error in (FileNotFoundError(), FW.PolicyError('invalid input')):
            firewall = MemoryFirewall(row(KEY, FW.rule_marker(TAG, KEY)))
            with patch.object(FW.os, 'geteuid', return_value=0), patch.object(FW, 'read_snapshot', side_effect=error), \
                 patch.object(FW, 'firewall_session', return_value=nullcontext(firewall)), self.assertLogs(level='ERROR'):
                self.assertEqual(FW.main(ARGS), 1)
            self.assertFalse(firewall.records)
        firewall = MemoryFirewall(row(KEY, FW.rule_marker(TAG, KEY)))
        with patch.object(FW.os, 'geteuid', return_value=0), patch.object(FW, 'read_snapshot') as read, \
             patch.object(FW, 'firewall_session', return_value=nullcontext(firewall)):
            self.assertEqual(FW.main(ARGS + ['--cleanup']), 0)
        read.assert_not_called()
        self.assertFalse(firewall.records)


if __name__ == '__main__':
    unittest.main()
