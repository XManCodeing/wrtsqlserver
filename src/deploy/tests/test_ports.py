"""Port and firewall transaction tests with isolated Docker/iptables/netstat/procd mocks."""
import json
import subprocess
import unittest
import test_deploy

MOCK_IPTABLES = r'''#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
args=sys.argv[1:]
if args[:2]==['-w','5']: args=args[2:]
assert args[:2]==['-t','mangle'], args
file=Path(os.environ['MOCK_RULES'])
rules=json.loads(file.read_text())
operation=args[2]
if operation=='-S':
    print('-P PREROUTING ACCEPT')
    print('\n'.join(rules)); sys.exit(0)
assert operation in ('-I','-D','-C'), args
start=5 if operation=='-I' else 4
tokens=args[start:]
def value(flag): return tokens[tokens.index(flag)+1]
destination=value('-d'); destination=destination if '/' in destination else destination+'/32'
rule='-A PREROUTING -s '+value('-s')+' -d '+destination+' -i '+value('-i')+' -p tcp '
if '--dports' in tokens: rule+='-m multiport --dports '+value('--dports')
else: rule+='-m tcp --dport '+value('--dport')
if '--comment' in tokens: rule+=' -m comment --comment '+value('--comment')
rule+=' -j RETURN'
if operation=='-I': rules.insert(0,rule)
elif rule not in rules: sys.exit(1)
elif operation=='-D': rules.remove(rule)
file.write_text(json.dumps(rules))
'''


class PortDeploymentTests(unittest.TestCase):
    """Fixtures are exclusively temporary and all host effects are mocked."""

    setUp = test_deploy.DeploymentTests.setUp
    executable = test_deploy.DeploymentTests.executable
    invoke = test_deploy.DeploymentTests.invoke
    commands = test_deploy.DeploymentTests.commands
    relay_fixture = test_deploy.DeploymentTests.relay_fixture

    def port_fixture(self):
        """Prepare an explicitly migrated firewall owner without publishing 3306."""
        relay=self.relay_fixture()
        home=self.usb/'relay-updater'
        home.mkdir(mode=0o700)
        marker=home/'lan-owned'; marker.write_text('lan-v1\n'); marker.chmod(0o600)
        self.rules=self.root/'rules.json'
        prefix='-A PREROUTING -s 192.168.10.0/24 -d 192.168.10.1/32 -i br-lan -p tcp -m tcp --dport '
        self.rules.write_text(json.dumps([prefix+str(port)+' -m comment --comment db-relay-lan-v1 -j RETURN' for port in (13306,18080)]+['-A PREROUTING -j foreign-proxy']))
        self.env['MOCK_RULES']=str(self.rules)
        self.executable('iptables',MOCK_IPTABLES)
        self.executable('netstat','#!/bin/sh\nprintf "%s\\n" "${MOCK_LISTENERS:-}"\n')
        self.executable('ss','#!/bin/sh\nprintf "%s\\n" "${MOCK_LISTENERS:-}"\n')
        return relay,home

    def test_ports_same_image_and_rollback(self):
        """New publications use immutable current image and restore paired policy/config/rules."""
        relay,home=self.port_fixture()
        result=self.invoke('relay-update.sh','ports-apply',RELAY_PORTS='3306,13306')
        self.assertEqual(result.returncode,0,result.stderr)
        run=next(command for command in self.commands() if command[0]=='run')
        self.assertEqual(run[-1],'sha256:old')
        self.assertIn('192.168.10.1:3306:3306/tcp',run)
        self.assertNotIn('pull',[command[0] for command in self.commands()])
        self.assertEqual((home/'ports.applied').read_text(),'3306,13306\n')
        result=self.invoke('relay-update.sh','rollback')
        self.assertEqual(result.returncode,0,result.stderr)
        self.assertEqual((home/'ports.applied').read_text(),'13306\n')
        self.assertEqual((relay/'data/config.enc').read_text(),'old-config')
        self.assertFalse(any('--dport 3306 ' in rule for rule in json.loads(self.rules.read_text())))

    def test_ports_zero_after_finalize_and_upgrade(self):
        """Zero business ports retains management and subsequent image updates preserve zero."""
        _,home=self.port_fixture()
        marker=home/'ports-migrated'; marker.write_text('yes\n'); marker.chmod(0o600)
        result=self.invoke('relay-update.sh','ports-apply',RELAY_PORTS='')
        self.assertEqual(result.returncode,0,result.stderr)
        self.assertEqual(self.invoke('relay-update.sh','finalize').returncode,0)
        result=self.invoke('relay-update.sh','apply',MOCK_ID='c'*64)
        self.assertEqual(result.returncode,0,result.stderr)
        run=[command for command in self.commands() if command[0]=='run'][-1]
        self.assertEqual(run.count('-p'),1)
        self.assertEqual((home/'ports.applied').read_text(),'\n')

    def test_ports_conflict_and_initial_baseline(self):
        """First migration needs both baseline ports and refuses occupied 3306 without mutation."""
        relay,_=self.port_fixture()
        for ports,listeners in [('13306',''),('3306,13306','tcp 0 0 0.0.0.0:3306 0.0.0.0:* LISTEN'),('6379,13306','')]:
            result=self.invoke('relay-update.sh','ports-check',RELAY_PORTS=ports,MOCK_LISTENERS=listeners)
            self.assertNotEqual(result.returncode,0)
            self.assertFalse((relay/'transaction').exists())
        self.assertFalse(any(command[0]=='stop' for command in self.commands()))

    def test_rules_order_and_unknown_marker(self):
        """Presence behind a proxy jump or malformed ownership is never considered ready."""
        self.port_fixture()
        rules=json.loads(self.rules.read_text())
        self.rules.write_text(json.dumps(rules[-1:]+rules[:-1]))
        result=self.invoke('relay-update.sh','ports-rules',RELAY_PORTS='13306')
        self.assertNotEqual(result.returncode,0)
        self.rules.write_text(json.dumps(['-A PREROUTING -m comment --comment db-relay-lan-v1 -j ACCEPT']))
        result=self.invoke('relay-update.sh','ports-rules',RELAY_PORTS='13306')
        self.assertNotEqual(result.returncode,0)

    def test_health_failure_restores_rules(self):
        """Failed candidate recovers original rules and policy via the shared rollback trap."""
        relay,home=self.port_fixture()
        result=self.invoke('relay-update.sh','ports-apply',RELAY_PORTS='3306,13306',FAIL_HEALTH='yes')
        self.assertNotEqual(result.returncode,0)
        self.assertTrue((relay/'transaction/rolled-back').exists(),result.stderr)
        self.assertEqual((home/'ports.applied').read_text(),'13306\n')

    def test_netstat_fallback_without_ss(self):
        """BusyBox netstat -lnt is sufficient; no additional ss package is required."""
        self.port_fixture()
        common=self.deploy/'ports-common.sh'
        common.write_text(common.read_text().replace('if command -v ss >/dev/null; then','if false; then'))
        result=self.invoke('relay-update.sh','ports-check',RELAY_PORTS='3306,13306')
        self.assertEqual(result.returncode,0,result.stderr)
        result=self.invoke('relay-update.sh','ports-check',RELAY_PORTS='3306,13306',MOCK_LISTENERS='tcp 0 0 :::3306 :::* LISTEN')
        self.assertNotEqual(result.returncode,0)

    def test_unknown_legacy_script_refused_readonly(self):
        """Migration never executes or takes over an unrecognized legacy script."""
        _,home=self.port_fixture()
        (home/'lan-owned').unlink()
        legacy=home/'lan-redis-bypass.sh'
        legacy.write_text('#!/bin/sh\necho unknown\n');legacy.chmod(0o700)
        result=self.invoke('lan-manage.sh','check',MOCK_TRUST_ROOT=str(self.root))
        self.assertNotEqual(result.returncode,0)
        self.assertIn('unknown legacy script',result.stderr)
        self.assertFalse((home/'lan-migration').exists())

    def legacy_fixture(self):
        """Prepare the confirmed script grammar, multiport rules and named UCI include."""
        _,home=self.port_fixture()
        (home/'lan-owned').unlink()
        legacy=home/'lan-redis-bypass.sh'
        original=(test_deploy.DEPLOY/'tests/fixtures/lan-redis-bypass.sh').read_text()
        legacy.write_text(original);legacy.chmod(0o700)
        prefix='-A PREROUTING -s 192.168.10.0/24 -d 192.168.10.1/32 -i br-lan -p tcp '
        redis=prefix+'-m tcp --dport 6379 -m comment --comment relay-lan-redis-bypass -j RETURN'
        app=prefix+'-m multiport --dports 18080,13306 -m comment --comment relay-lan-app-bypass -j RETURN'
        self.rules.write_text(json.dumps([app,redis,'-A PREROUTING -j foreign-proxy']))
        self.executable('uci', '''#!/bin/sh
case "$*" in
  '-q get firewall.relay_lan_redis') printf 'include\\n';;
  '-q get firewall.relay_lan_redis.path') printf '%s/relay-updater/lan-redis-bypass.sh\\n' "$MOCK_ROOT";;
  '-q get firewall.relay_lan_redis.type') printf 'script\\n';;
  '-q get firewall.relay_lan_redis.reload'|'-q get firewall.relay_lan_redis.enabled') printf '1\\n';;
  '-q get firewall.db_relay_ports') exit 1;;
  'changes firewall') exit 0;;
  'show firewall') printf "firewall.relay_lan_redis.path='%s/relay-updater/lan-redis-bypass.sh'\\n" "$MOCK_ROOT";;
  'export firewall') printf 'config include relay_lan_redis\\n';;
  'set '*|'commit firewall') exit 0;;
  *) exit 99;;
esac
''')
        return home,legacy,original,redis,app

    def test_known_legacy_migration_backup_and_redis_preserved(self):
        """Takeover removes only the exact multiport rule and preserves original Redis bytes."""
        home,legacy,original,redis,app=self.legacy_fixture()
        result=self.invoke('lan-manage.sh','install',MOCK_TRUST_ROOT=str(self.root))
        self.assertEqual(result.returncode,0,result.stderr)
        self.assertEqual((home/'lan-migration/legacy-script').read_text(),original)
        rules=json.loads(self.rules.read_text())
        self.assertIn(redis,rules)
        self.assertIn('-A PREROUTING -j foreign-proxy',rules)
        self.assertTrue((home/'lan-migration/firewall-uci').exists())
        self.assertEqual((home/'lan-owned').read_text(),'lan-v1\n')
        self.assertNotIn('relay-lan-app-bypass',legacy.read_text())
        self.assertTrue(original.startswith(legacy.read_text()))
        self.assertNotIn(app,rules)
        self.assertFalse(any(command[0]=='run' for command in self.commands()))

    def test_migration_rejects_pending_and_existing_uci(self):
        """Pending user changes and existing destination include are never overwritten."""
        home,legacy,original,_,_=self.legacy_fixture()
        uci=self.bin/'uci'; clean=uci.read_text()
        for changed in [clean.replace("'changes firewall') exit 0", "'changes firewall') printf 'pending\\n'"),
                        clean.replace("'-q get firewall.db_relay_ports') exit 1", "'-q get firewall.db_relay_ports') printf 'include\\n'")]:
            uci.write_text(changed)
            result=self.invoke('lan-manage.sh','install',MOCK_TRUST_ROOT=str(self.root))
            self.assertNotEqual(result.returncode,0)
            self.assertEqual(legacy.read_text(),original)
            self.assertFalse((home/'lan-migration').exists())

    def test_migration_partial_failure_retains_manual_evidence(self):
        """Failed UCI commit retains durable manual-recovery state and original snapshots."""
        home,_,original,redis,_=self.legacy_fixture()
        uci=self.bin/'uci'
        uci.write_text(uci.read_text().replace("'set '*|'commit firewall') exit 0", "'commit firewall') exit 7;;\n  'set '*) exit 0"))
        result=self.invoke('lan-manage.sh','install',MOCK_TRUST_ROOT=str(self.root))
        self.assertNotEqual(result.returncode,0)
        self.assertEqual((home/'lan-migration/state').read_text(),'manual_recovery\n')
        self.assertEqual((home/'lan-migration/legacy-script').read_text(),original)
        self.assertIn(redis,json.loads(self.rules.read_text()))
        self.assertFalse((home/'lan-owned').exists())

    def test_legacy_parser_rejects_executable_injections(self):
        """No substitutions, sourced files or appended commands can enter the accepted prefix."""
        home,legacy,original,_,_=self.legacy_fixture()
        for payload in ['\n. /tmp/unsafe\n','\necho "$(touch /tmp/unsafe)"\n','\niptables -t nat -F\n']:
            legacy.write_text(original+payload)
            result=self.invoke('lan-manage.sh','check',MOCK_TRUST_ROOT=str(self.root))
            self.assertNotEqual(result.returncode,0)
            self.assertFalse((home/'lan-migration').exists())

    def test_scope_data_is_never_sourced(self):
        """Private but malformed LAN scope cannot execute shell commands at lifecycle entry."""
        _,home=self.port_fixture()
        scope=home/'lan-scope'
        sentinel=self.root/'injection-ran'
        for content in [f'LAN_INTERFACE=br-lan\nLAN_BIND_IP=$(touch {sentinel})\n',
                        'LAN_INTERFACE=br-lan\nLAN_BIND_IP=192.168.10.1\nLAN_INTERFACE=wan\n',
                        'LAN_INTERFACE=br-lan\nLAN_BIND_IP=192.168.10.1;false\n']:
            scope.write_text(content);scope.chmod(0o600)
            result=self.invoke('lan-restore.sh')
            self.assertNotEqual(result.returncode,0)
            self.assertFalse(sentinel.exists())

    def test_procd_install_is_explicit_and_mocked(self):
        """Installer writes only a temporary init target and never runs a real service manager."""
        self.port_fixture()
        script=self.deploy/'updater-service.sh'
        text=script.read_text().replace('INIT=/etc/init.d/relay-updater','INIT=$MOCK_ROOT/mock-init')
        text=text.replace('\nservice_check\ncase', '\nservice_check() { [ "$USB" = "$MOCK_ROOT" ]; }\nservice_check\ncase')
        script.write_text(text)
        result=self.invoke('updater-service.sh','check')
        self.assertEqual(result.returncode,0,result.stderr)
        self.assertFalse((self.usb/'mock-init').exists())
        result=self.invoke('updater-service.sh','install')
        self.assertEqual(result.returncode,0,result.stderr)
        installed=(self.usb/'mock-init').read_text()
        self.assertIn('START=99',installed)
        self.assertIn('procd_set_param command',installed)
        self.assertNotIn('setsid',installed)
        self.assertNotEqual(self.invoke('updater-service.sh','install').returncode,0)
