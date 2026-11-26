#!/usr/bin/python3
"""One-shot Ubuntu host reconciliation. Requires distro Python, lock-aware UFW and root.

The app's file is a proposal, never a host command. Socket ownership is a point-in-time
check: UFW rules cannot bind to a process lifetime. Run periodically and on file changes.
"""
import argparse
from collections import defaultdict
from contextlib import contextmanager, ExitStack, redirect_stderr
import fcntl
import gettext
import hashlib
import ipaddress
import json
import logging
import os
from pathlib import Path
import re
import selectors
import stat
import subprocess
import sys
import time


DOCKER = ['/usr/bin/docker', '--host', 'unix:///var/run/docker.sock']
SS = '/usr/bin/ss'
ENV = {'PATH': '/usr/sbin:/usr/bin:/sbin:/bin', 'LC_ALL': 'C', 'HOME': '/root'}
MAX_FILE = 256 * 1024
MAX_OUTPUT = 1024 * 1024
MAX_RULES = 256
MAX_MUTATIONS = 64
V4 = ipaddress.IPv4Network
IP = ipaddress.IPv4Address
PRIVATE = tuple(map(V4, ('10.0.0.0/8', '172.16.0.0/12', '192.168.0.0/16')))
RESERVED = {6379, 8080, 18080}
DIRECTORY = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW
# Project only the required metadata. Never return arbitrary Config.Env/labels/secrets.
INSPECT = ('{{.Id}}\n{{.State.Status}}\n{{.State.Pid}}\n{{.State.StartedAt}}\n'
           '{{.HostConfig.NetworkMode}}\n{{.Path}}\n'
           '{{index .Config.Labels "com.docker.compose.project"}}\n'
           '{{index .Config.Labels "com.docker.compose.service"}}\n'
           '{{index .Config.Labels "com.docker.compose.project.working_dir"}}\n'
           '{{index .Config.Labels "io.db-relay.deploy.managed"}}\n'
           '{{range .Mounts}}{{if eq .Destination "/data"}}{{println .Type .Source}}{{end}}{{end}}'
           '{{range .Config.Env}}{{if eq (index (split . "=") 0) "LISTEN_ADDR"}}{{println .}}{{end}}{{end}}')


class PolicyError(RuntimeError):
    """A fixed, safe diagnostic suitable for the system journal."""


def require(condition, message):
    """Raise the supplied static diagnostic unless condition holds; return None."""
    if not condition:
        raise PolicyError(message)


def project_path(raw):
    """Validate an absolute deployment directory below a supported prefix; return Path."""
    path = Path(raw)
    require(str(path) == raw and len(raw) <= 512 and re.fullmatch(r'/[A-Za-z0-9_/-]+', raw)
            and '..' not in path.parts and any(parent in path.parents for parent in
            map(Path, ('/www/wwwroot', '/opt', '/srv'))), 'unsafe project directory')
    return path


def lan_policy(raw_ip, raw_cidr):
    """Validate operator LAN arguments; return private unicast IPv4 and strict subnet."""
    address, network = IP(raw_ip), V4(raw_cidr)
    require('/' in raw_cidr and any(network.subnet_of(block) for block in PRIVATE)
            and address in network and (network.prefixlen >= 31 or address not in
            (network.network_address, network.broadcast_address)), 'invalid RFC1918 LAN scope')
    return address, network


def trusted_dir(fd):
    """Reject non-root or writable deployment ancestors referenced by fd; return None."""
    info = os.fstat(fd)
    require(info.st_uid == 0 and not info.st_mode & 0o022, 'untrusted deployment directory')


def read_snapshot(project):
    """Read fixed data path through pinned Linux descriptors; never open a device/FIFO."""
    with ExitStack() as stack:
        current = os.open('/', DIRECTORY)
        stack.callback(os.close, current)
        trusted_dir(current)
        for index, segment in enumerate((*project.parts[1:], 'data', 'relay')):
            current = os.open(segment, DIRECTORY, dir_fd=current)
            stack.callback(os.close, current)
            # Only the final /data mount may belong to the unprivileged application.
            if index < len(project.parts):
                trusted_dir(current)
            else:
                info = os.fstat(current)
                require(info.st_uid in (0, 10001) and not info.st_mode & 0o022, 'unsafe relay data directory')
        entry = os.open('firewall-listeners.json', os.O_PATH | os.O_NOFOLLOW, dir_fd=current)
        stack.callback(os.close, entry)
        info = os.fstat(entry)
        require(stat.S_ISREG(info.st_mode) and info.st_nlink == 1 and info.st_uid in (0, 10001)
                and not info.st_mode & 0o022 and info.st_size <= MAX_FILE,
                'unsafe snapshot type, owner, mode, links or size')
        with open('/proc/self/fd/' + str(entry), 'rb') as stream:
            content = stream.read(MAX_FILE + 1)
            after = os.fstat(stream.fileno())
        require(len(content) == info.st_size and after.st_mtime_ns == info.st_mtime_ns
                and after.st_ctime_ns == info.st_ctime_ns, 'snapshot changed during read')
        return content


def unique_object(pairs):
    """Create a JSON object from pairs, rejecting duplicate field names."""
    result = dict(pairs)
    require(len(pairs) == len(result), 'duplicate snapshot fields')
    return result


def candidates(content, address, lan):
    """Validate untrusted snapshot bytes; return {(host,port): bounded LAN source subnets}."""
    try:
        document = json.loads(content, object_pairs_hook=unique_object)
        require(isinstance(document, dict) and set(document) == {'version', 'listeners'}
                and type(document['version']) is int and document['version'] == 1,
                'unsupported snapshot schema')
        listeners = document['listeners']
        require(isinstance(listeners, list) and len(listeners) <= 256, 'invalid listener count')
        result, seen = {}, set()
        for row in listeners:
            require(isinstance(row, dict) and set(row) == {'host', 'port', 'sources'}, 'invalid listener fields')
            require(isinstance(row['host'], str) and type(row['port']) is int, 'invalid listener endpoint')
            host, port = IP(row['host']), row['port']
            require(1 <= port <= 65535 and (host, port) not in seen, 'invalid or duplicate endpoint')
            seen.add((host, port))
            sources = row['sources']
            require(isinstance(sources, list) and len(sources) <= 64, 'invalid source count')
            allowed = set()
            for value in sources:
                require(isinstance(value, str) and re.fullmatch(r'[0-9.]+/[0-9]{1,2}', value), 'invalid source CIDR')
                source = V4(value, strict=False)
                if source.overlaps(lan):
                    allowed.add(source if source.subnet_of(lan) else lan)
            if host in (IP(0), address) and port >= 1024 and port not in RESERVED and allowed:
                result[(str(host), port)] = set(ipaddress.collapse_addresses(allowed))
        require(sum(map(len, result.values())) <= MAX_RULES, 'too many requested firewall rules')
        return result
    except (ValueError, UnicodeError, RecursionError) as error:
        raise PolicyError('invalid firewall snapshot') from error


def command(argv):
    """Run only fixed local tools with argument vectors; return bounded stdout, no secrets."""
    require(argv[0] in (DOCKER[0], SS), 'unexpected executable')
    deadline = time.monotonic() + 10
    with subprocess.Popen(argv, cwd='/', env=ENV, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                          stderr=subprocess.DEVNULL) as process, selectors.DefaultSelector() as selector:
        try:
            selector.register(process.stdout, selectors.EVENT_READ)
            output = bytearray()
            while True:
                require(selector.select(max(0, deadline - time.monotonic())) and time.monotonic() < deadline,
                        'host inspection timed out')
                chunk = os.read(process.stdout.fileno(), 65536)
                if not chunk:
                    break
                output.extend(chunk)
                require(len(output) <= MAX_OUTPUT, 'host inspection output limit exceeded')
            require(process.wait(timeout=max(0, deadline - time.monotonic())) == 0, 'host inspection command failed')
            return output.decode('utf-8')
        finally:
            if process.poll() is None:
                process.kill()


def container(project):
    """Resolve exactly one running Compose relay; return identity, PID, start time and API port."""
    ids = command(DOCKER + ['ps', '--no-trunc', '-q', '--filter', 'label=com.docker.compose.project=db-relay',
                            '--filter', 'label=com.docker.compose.service=db-relay']).split()
    if not ids:
        return None
    require(len(ids) == 1 and re.fullmatch('[0-9a-f]{64}', ids[0]), 'ambiguous relay container identity')
    fields = command(DOCKER + ['inspect', '--type', 'container', '--format', INSPECT, ids[0]]).strip().splitlines()
    require(len(fields) == 12 and fields[0:2] == [ids[0], 'running'] and fields[2].isdecimal()
            and int(fields[2]) > 1 and fields[3] and fields[4:11] == ['host', '/app/db-relay', 'db-relay',
            'db-relay', str(project), 'relay-ubuntu-v1', 'bind ' + str(project / 'data/relay')],
            'container does not match running host relay or its snapshot mount')
    manage = re.fullmatch(r'LISTEN_ADDR=[^\s]+:([0-9]{1,5})', fields[11])
    require(manage and 1 <= int(manage[1]) <= 65535, 'ambiguous management listener')
    return ids[0], int(fields[2]), fields[3], int(manage[1])


def live_rules(project, requested, address):
    """Cross-check Docker and root ss: only unique exact tcp4 sockets of the relay PID survive."""
    if not requested:
        return set()
    identity = container(project)
    if identity is None:
        logging.info('relay stopped; withdrawing owned rules')
        return set()
    _, pid, _, management = identity
    matches = defaultdict(list)
    for family in ('-4', '-6'):
        listing = command([SS, '-H', '-n', '-l', '-t', '-p', family])
        for line in listing.splitlines():
            columns = line.split(None, 5)
            require(len(columns) >= 5 and columns[0] == 'LISTEN', 'unrecognized ss listener row')
            endpoint, separator, raw_port = columns[3].rpartition(':')
            require(separator and raw_port.isdecimal(), 'unrecognized ss endpoint')
            port = int(raw_port)
            # Conservatively exclude a second socket on this destination (including v6 wildcard,
            # mapped IPv4, reuseport and interface-bound sockets), regardless of the reported owner.
            endpoint = endpoint.split('%', 1)[0].strip('[]')
            if family == '-6':
                if endpoint in ('*', '::') or ipaddress.IPv6Address(endpoint).ipv4_mapped in (address, IP(0)):
                    matches[port].append(None)
                continue
            if endpoint == '*':
                endpoint = '0.0.0.0'
            require(str(IP(endpoint)) == endpoint, 'unrecognized ss IPv4 address')
            if endpoint not in ('0.0.0.0', str(address)):
                continue
            owners = columns[5].strip() if len(columns) == 6 else ''
            owner = re.fullmatch(r'users:\(\("db-relay",pid=([0-9]+),fd=[0-9]+\)\)', owners)
            matches[port].append(endpoint if owner and int(owner[1]) == pid and '%' not in columns[3] else None)
    require(container(project) == identity, 'relay restarted during socket verification')
    return {(address, port, source) for (host, port), sources in requested.items()
            if matches[port] == [host] and port != management for source in sources}


def marker(project):
    """Return the stable, project-scoped, exact UFW ownership comment."""
    return 'db-relay-fw-v1-' + hashlib.sha256(str(project).encode('ascii')).hexdigest()[:24]


def rule_key(rule):
    """Return canonical address/port/source for comparable plain input rules, else None."""
    if (rule.v6 or rule.forward or rule.direction != 'in' or rule.protocol != 'tcp' or rule.sport != 'any'
            or rule.dapp or rule.sapp or rule.interface_in or rule.interface_out or not rule.dport.isdecimal()):
        return None
    dst, src = V4(rule.dst, strict=False), V4(rule.src, strict=False)
    if dst.prefixlen == 32:
        return dst.network_address, int(rule.dport), src
    return None


def rule_marker(tag, key):
    """Bind the project tag to a canonical tuple so a copied comment cannot claim another rule."""
    dst, port, src = key
    fingerprint = hashlib.sha256(f'{dst}|{port}|{src}'.encode('ascii')).hexdigest()[:16]
    return tag + '-' + fingerprint


def classify(rules, tag):
    """Index literal v4 rule objects without losing duplicates or interpreting manual comments."""
    require(len(rules) <= 4096, 'UFW inventory exceeds safe limit')
    rows = defaultdict(list)
    for rule in rules:
        # UFW stores the real comment as hex. Do not rely on display text: a manual comment
        # can contain a fake status row, the tag as a substring, or a second '#' character.
        key = rule_key(rule)
        plain = rule.action == 'allow' and not rule.logtype
        owned = bool(key and rule.comment == rule_marker(tag, key).encode('ascii').hex())
        if owned:
            require(key and plain and key[1] >= 1024 and key[1] not in RESERVED
                    and any(key[0] in block and key[2].subnet_of(block) for block in PRIVATE),
                    'unexpected tagged UFW rule; refusing ownership')
        if key:
            rows[key].append((owned, plain))
    return rows


class DistroDiagnostics:
    """Capture the presence, not contents, of warnings from the distro UFW frontend."""

    def __init__(self):
        """Initialize an empty, constant-size diagnostic indicator."""
        self.emitted = False

    def write(self, text):
        """Discard text potentially containing hand-written rules and return text length."""
        self.emitted |= bool(text.strip())
        return len(text)

    def flush(self):
        """Implement the file-like interface; no buffered data needs flushing."""
        return None


def distro_call(function, *args):
    """Run UFW function with sanitized diagnostics; never proceed after a skipped/malformed rule."""
    diagnostics = DistroDiagnostics()
    try:
        with redirect_stderr(diagnostics):
            result = function(*args)
    except SystemExit as error:
        raise PolicyError('Ubuntu UFW rejected the operation') from error
    require(not diagnostics.emitted, 'UFW reported a warning; refusing ambiguous state')
    return result


class Firewall:
    """Narrow facade over Ubuntu's own UFW CLI parser and iptables backend."""

    def __init__(self):
        """Initialize Ubuntu UFW only inside the host's lock, never from project directories."""
        import ufw.frontend
        gettext.install('ufw')
        self.make_frontend = ufw.frontend.UFWFrontend
        self.frontend = None
        self.parse = ufw.frontend.parse_command

    def rules(self):
        """Reload persisted rows; check the live 'status numbered' header, never its lossy text."""
        self.frontend = distro_call(self.make_frontend, False)
        backend = self.frontend.backend
        rows = backend.get_rules()
        require(len(rows) <= 4096, 'UFW inventory exceeds safe limit')
        require(backend.is_enabled() and distro_call(backend.get_status, False, True).partition('\n')[0] == 'Status: active',
                'UFW is inactive or unrecognized; not enabling it')
        return rows

    def mutate(self, key, tag, delete):
        """Apply one exact comment-bearing numeric IPv4 rule, never a numbered delete."""
        dst, port, src = key
        tag = rule_marker(tag, key)
        source_arg = str(src.network_address) if src.prefixlen == 32 else str(src)
        argv = ['ufw'] + (['delete'] if delete else []) + ['allow', 'in', 'proto', 'tcp', 'from', source_arg,
                                                           'to', str(dst), 'port', str(port), 'comment', tag]
        parsed = distro_call(self.parse, argv)
        rule = parsed.data['rule']
        require(parsed.action == 'allow' and parsed.data['iptype'] == 'v4' and bool(rule.remove) == delete
                and rule.comment == tag.encode('ascii').hex() and rule_key(rule) == key,
                'UFW parser does not support exact tagged IPv4 rules')
        result = distro_call(self.frontend.do_action, parsed.action, rule, 'v4', False).strip()
        require(result in (('Rule deleted', 'Rule updated') if delete else ('Rule added', 'Rule updated')),
                'UFW did not acknowledge rule change')


@contextmanager
def firewall_session():
    """Hold UFW's native POSIX lock across inspection and mutation; yield a fresh facade."""
    # UFW 0.36.2 uses /run/ufw.lock (fcntl.lockf), not flock. Older releases without
    # the native locking API cannot safely coordinate check-then-add with other clients.
    import ufw.util
    require(callable(getattr(ufw.util, 'create_lock', None)), 'UFW version lacks host transaction lock')
    fd = os.open('/run/ufw.lock', os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW | os.O_NONBLOCK, 0o600)
    try:
        info = os.fstat(fd)
        require(stat.S_ISREG(info.st_mode) and info.st_uid == 0 and not info.st_mode & 0o022,
                'unsafe UFW lock file')
        fcntl.lockf(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        yield Firewall()
    finally:
        os.close(fd)


def synchronize(firewall, desired, tag):
    """Remove stale owned rules first, then append missing rules; verify all observed changes."""
    rows = classify(firewall.rules(), tag)
    owned = {key for key, values in rows.items() if any(item[0] for item in values)}
    ambiguous = {key for key in owned | desired if len(rows.get(key, [])) > 1
                 or (key in desired and rows.get(key) == [(False, False)])}
    removals = sorted(owned - desired - ambiguous)
    additions = sorted(desired - rows.keys())
    failed = bool(ambiguous)
    if failed:
        logging.error('ambiguous/manual conflicting UFW rules; unsafe changes withheld')
    for key in removals[:MAX_MUTATIONS]:
        try:
            firewall.mutate(key, tag, True)
        except Exception as error:
            failed = True
            logging.error('owned rule cleanup failed for TCP %d (%s)', key[1], type(error).__name__)
    require(len(removals) <= MAX_MUTATIONS, 'cleanup batch limit reached; retry required')
    require(not failed, 'UFW cleanup or ownership verification incomplete')
    remaining = MAX_MUTATIONS - len(removals)
    for key in additions[:remaining]:
        firewall.mutate(key, tag, False)
    require(len(additions) <= remaining, 'addition batch limit reached; retry required')
    if additions or removals:
        after = classify(firewall.rules(), tag)
        require(all(key not in after for key in removals)
                and all(after.get(key) == [(True, True)] for key in additions), 'UFW postcondition failed')
        logging.info('UFW reconciled: %d removed, %d added', len(removals), len(additions))


def reconcile(project, address, lan, cleanup):
    """Reconcile one validated project; withdraw provably owned rules on bad/unavailable evidence."""
    tag, problem, wanted = marker(project), None, set()
    with firewall_session() as firewall:
        if not cleanup:
            try:
                requested = candidates(read_snapshot(project), address, lan)
                wanted = live_rules(project, requested, address)
            except Exception as error:
                problem = error
                logging.error('snapshot/listener evidence unavailable (%s); cleaning owned rules',
                              str(error) if isinstance(error, PolicyError) else type(error).__name__)
        try:
            synchronize(firewall, wanted, tag)
        except Exception:
            logging.error('UFW reconciliation incomplete; inspect owned rules and retry')
            raise
    require(problem is None, 'source verification failed; owned rules cleaned')


def main(argv=None):
    """Parse the fixed oneshot CLI; return 0 on convergence, 1 on runtime failure (usage: 2)."""
    parser = argparse.ArgumentParser(description=__doc__, allow_abbrev=False)
    parser.add_argument('--project-dir', required=True)
    parser.add_argument('--lan-ip', required=True)
    parser.add_argument('--lan-cidr', required=True)
    parser.add_argument('--cleanup', action='store_true')
    args = parser.parse_args(argv)
    try:
        require(os.geteuid() == 0, 'root required')
        project = project_path(args.project_dir)
        address, lan = lan_policy(args.lan_ip, args.lan_cidr)
        reconcile(project, address, lan, args.cleanup)
    except Exception as error:
        logging.error('%s', str(error) if isinstance(error, PolicyError) else type(error).__name__)
        return 1
    return 0


if __name__ == '__main__':
    logging.basicConfig(level=logging.INFO, format='%(levelname)s: %(message)s')
    os.environ.clear()
    os.environ.update(ENV)
    sys.exit(main())
