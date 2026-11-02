"""CI-only smoke test of an explicitly named local image; never run on the router."""
import json
import os
from pathlib import Path
import secrets
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request


def docker(*arguments):
    """Run Docker with a bounded timeout; return stdout, raising on failure."""
    return subprocess.run(['docker', *arguments], check=True, text=True,
                          capture_output=True, timeout=90).stdout.strip()


def wait_http(base):
    """Wait at most 30 seconds for the unauthenticated session health endpoint."""
    for attempt in range(30):
        try:
            with urllib.request.urlopen(base + '/api/session', timeout=1) as response:
                assert json.load(response) == {'authenticated': False}
                return
        except (urllib.error.URLError, TimeoutError):
            if attempt == 29:
                raise RuntimeError('Image session health timed out') from None
            time.sleep(1)


def verify(base, password):
    """Check static UI, login and protected status without logging any credentials."""
    with urllib.request.urlopen(base + '/', timeout=5) as response:
        assert b'<html' in response.read(1 << 20)
    request = urllib.request.Request(base + '/api/login',
                                    data=json.dumps({'password': password}).encode(),
                                    headers={'Content-Type': 'application/json', 'Origin': base})
    with urllib.request.urlopen(request, timeout=5) as response:
        assert 'csrfToken' in json.load(response)
        cookie = response.headers['Set-Cookie'].split(';', 1)[0]
    for endpoint in ('/api/status', '/api/redis/status', '/api/online-updates'):
        request = urllib.request.Request(base + endpoint, headers={'Cookie': cookie})
        with urllib.request.urlopen(request, timeout=5) as response:
            payload = json.load(response)
            if endpoint == '/api/online-updates':
                assert payload['enabled'] is False


def main(image):
    """Create one isolated CI container and remove only that exact container afterward."""
    if os.environ.get('GITHUB_ACTIONS') != 'true':
        raise RuntimeError('This test must run on the disposable GitHub Actions runner')
    name = 'relay-ci-' + secrets.token_hex(6)
    with tempfile.TemporaryDirectory() as temporary:
        root = Path(temporary)
        root.chmod(0o755)
        data = root / 'data'
        data.mkdir(mode=0o700)
        password = secrets.token_urlsafe(32)
        secret = root / 'password'
        secret.write_text(password)
        secret.chmod(0o600)
        subprocess.run(['sudo', 'chown', '10001:10001', str(data), str(secret)], check=True)
        try:
            docker('run', '-d', '--name', name, '--pull', 'never', '--user', '10001:10001',
                   '--read-only', '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges:true',
                   '--memory', '128m', '--pids-limit', '64', '--tmpfs', '/tmp:rw,noexec,nosuid,size=8m',
                   '-v', f'{data}:/data', '-v', f'{secret}:/run/secrets/admin_password:ro',
                   '-e', 'ADMIN_PASSWORD_FILE=/run/secrets/admin_password',
                   '-p', '127.0.0.1::8080', image)
            port = json.loads(docker('inspect', '--format', '{{json .NetworkSettings.Ports}}', name))['8080/tcp'][0]['HostPort']
            base = 'http://127.0.0.1:' + port
            wait_http(base)
            verify(base, password)
            print('Image smoke test passed: nonroot/read-only startup, login, status and disabled updater')
        finally:
            try:
                docker('rm', '-f', name)
            finally:
                subprocess.run(['sudo', 'chown', '-R', f'{os.getuid()}:{os.getgid()}', str(root)], check=True)


if __name__ == '__main__':
    if len(sys.argv) != 2:
        raise SystemExit('usage: image_smoke.py IMAGE')
    main(sys.argv[1])
