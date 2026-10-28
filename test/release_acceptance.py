"""Release-artifact acceptance test (docs/releases.md#acceptance-test).

Installs the release archives, chart and images the way a user does: the CLI is extracted from its
archive into an empty directory, and no checkout, Go, Make or image build is involved in what is
being tested. Every phase uses its own disposable k3d cluster and deletes it afterwards; existing
clusters, kubeconfigs and labs are not touched.

  python3 test/release_acceptance.py --dist DIR --version VERSION [--images SOURCE] [PHASE ...]

DIR holds the archives, SHA256SUMS and panko-VERSION.tgz of one release (downloaded, or built with
tools/release-build.sh). SOURCE is where the images come from: `ghcr.io/hingst-it` (default, the
chart's own references) or `docker` for candidate images tagged
ghcr.io/hingst-it/<image>:VERSION in the local Docker daemon, which are imported or copied into a
private registry instead of pulled. The phases (default: all but gitops, which needs the published
OCI chart):

  server   panko-server archive (binary, systemd unit) and the agent image's versions
  mirror   module mirror waits for a pinned revision; runner fetches retry while it does
  dev      panko dev up/status/down from an empty directory, lifecycle, restart, failure and retry
  install  Helm directly, panko install (repeated), a module upgrade, private registry with pull
           secrets, the packaged remote agent (test/remote_e2e.py), uninstall, retention, reinstall
  gitops   Argo CD and Flux installing the published OCI chart; retention after deletion
  migrate  a v0.0.1 installation (from its tag) migrated with docs/upgrading.md, then update and destroy

Logs go to stdout; tokens, kubeconfigs and Secret contents are only written to private files under
the work directory and never printed.
"""
import argparse
import base64
import hashlib
import json
import os
import platform
import shutil
import socket
import subprocess
import tarfile
import tempfile
import time
import traceback
import urllib.error
import urllib.request
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parent.parent
REGISTRY = 'ghcr.io/hingst-it'
MODULE = 'https://github.com/hingst-it/panko-database.git'
# Two published commits of the example module, for the module upgrade.
OLD_MODULE, MODULE_REVISION = '23dfa930cd81302a8127cd9afb5b101a4aeb8cb2', '8304ffda663aa0943890cd1d8966a8c50b1fdc05'
ARGOCD = 'v3.5.6'
FLUX = 'v2.10.0'
REGISTRY_IMAGE = 'registry:2.8.3'
CRANE = 'gcr.io/go-containerregistry/crane/debug:v0.22.1'
POSTGRES_PATCH_KEY = 'password'


def log(message):
    print(time.strftime('%H:%M:%S ') + message, flush=True)


def run(command, *, env=None, cwd=None, check=True, input=None, timeout=1800, quiet=False):
    result = subprocess.run([str(c) for c in command], env=env, cwd=cwd, input=input, text=True, capture_output=True, timeout=timeout)
    if not quiet and result.stdout.strip():
        print(result.stdout.rstrip(), flush=True)
    if check and result.returncode != 0:
        raise AssertionError(f'{command[0]} {command[1] if len(command) > 1 else ""} failed ({result.returncode}): {result.stderr.strip()[-2000:]}')
    return result


def wait(check, what, timeout=600, interval=5):
    deadline = time.time() + timeout
    while True:
        result = check()
        if result:
            return result
        if time.time() > deadline:
            raise AssertionError(f'timed out waiting for {what}')
        time.sleep(interval)


def tool(name):
    """A user's own tool (k3d, helm, kubectl) for the parts a user brings: an existing cluster and Helm."""
    path = shutil.which(name) or run(['mise', 'which', name], cwd=ROOT, quiet=True).stdout.strip()
    assert path, f'{name} is required'
    return path


def digest(data):
    return hashlib.sha256(data if isinstance(data, bytes) else data.encode()).hexdigest()


def free_port():
    with socket.socket() as s:
        s.bind(('127.0.0.1', 0))
        return s.getsockname()[1]


class Release:
    def __init__(self, dist, version, images, work):
        self.dist, self.version, self.images, self.work = Path(dist).resolve(), version, images, work
        self.checksums = {}
        for line in (self.dist / 'SHA256SUMS').read_text().splitlines():
            value, name = line.split()
            self.checksums[name] = value
        system = {'Darwin': 'darwin', 'Linux': 'linux'}[platform.system()]
        machine = {'arm64': 'arm64', 'aarch64': 'arm64', 'x86_64': 'amd64', 'AMD64': 'amd64'}[platform.machine()]
        self.bin = work / 'bin'
        self.bin.mkdir(parents=True)
        self.extract(f'panko_{version}_{system}_{machine}.tar.gz', self.bin)
        self.chart = self.verified(f'panko-{version}.tgz')
        # The CLI's own environment: no Go, Make, k3d or Helm on PATH; Docker is a prerequisite.
        tools = work / 'docker-only'
        tools.mkdir()
        (tools / 'docker').symlink_to(shutil.which('docker'))
        self.env = {**os.environ, 'PATH': f'{self.bin}:{tools}:/usr/bin:/bin:/usr/sbin:/sbin', 'PANKO_HOME': str(work / 'panko-home')}
        for key in ('KUBECONFIG', 'PANKO_URL', 'PANKO_TOKEN', 'PANKO_TOKENS_FILE', 'PANKO_KUBECONFIG', 'GOPATH', 'GOROOT'):
            self.env.pop(key, None)
        output = self.panko('--version').stdout.strip()
        assert output == f'panko v{version}', output
        log(f'ok {output} from the release archive in an empty directory')

    def verified(self, name):
        path = self.dist / name
        assert digest(path.read_bytes()) == self.checksums[name], f'checksum mismatch for {name}'
        return path

    def extract(self, name, target):
        with tarfile.open(self.verified(name)) as archive:
            archive.extractall(target, filter='data')

    def panko(self, *args, cwd=None, check=True, timeout=1800, quiet=False, input=''):
        return run([self.bin / 'panko', *args], env=self.env, cwd=cwd or self.work / 'empty', check=check, timeout=timeout, quiet=quiet, input=input)

    def image(self, name):
        return f'{REGISTRY}/{name}:{self.version}'


class Cluster:
    """A user's existing cluster: a plain k3d cluster with its own kubeconfig."""

    def __init__(self, release, name, *create_args):
        self.release, self.name = release, name
        self.kubeconfig = release.work / f'{name}.kubeconfig'
        self.k3d = tool('k3d')
        existing = run([self.k3d, 'cluster', 'list', '-o', 'json'], quiet=True).stdout
        assert name not in [c['name'] for c in json.loads(existing)], f'k3d cluster {name} already exists; remove it or choose another name'
        env = {**os.environ, 'KUBECONFIG': str(self.kubeconfig)}
        run([self.k3d, 'cluster', 'create', name, '--wait', '--timeout', '180s', '--kubeconfig-update-default=false',
             '--kubeconfig-switch-context=false', '--k3s-arg', '--disable=traefik@server:0', *create_args], env=env, quiet=True)
        self.kubeconfig.write_text(run([self.k3d, 'kubeconfig', 'get', name], quiet=True).stdout)
        self.kubeconfig.chmod(0o600)
        import_candidate_images(release, name)
        log(f'ok cluster {name}')

    def kubectl(self, *args, check=True, input=None, quiet=True):
        return run([tool('kubectl'), '--kubeconfig', self.kubeconfig, *args], check=check, input=input, quiet=quiet)

    def get(self, kind, name, namespace=None):
        args = ['get', kind, name, '-o', 'json'] + (['-n', namespace] if namespace else [])
        result = self.kubectl(*args, check=False)
        return json.loads(result.stdout) if result.returncode == 0 else None

    def delete(self):
        run([self.k3d, 'cluster', 'delete', self.name], quiet=True, check=False)


def import_candidate_images(release, cluster):
    """Candidate images from the local Docker daemon stand in for the registry's."""
    if release.images == 'docker':
        for name in ('panko-server', 'panko-module-mirror'):
            run([tool('k3d'), 'image', 'import', release.image(name), '--cluster', cluster], quiet=True)


class Gateway:
    """The API through a port-forward, with the team tokens from a private file."""

    def __init__(self, kubeconfig, tokens):
        self.port = free_port()
        self.url = f'http://127.0.0.1:{self.port}'
        self.tokens = Path(tokens)
        self.process = subprocess.Popen([tool('kubectl'), '--kubeconfig', kubeconfig, '-n', 'infra-gateway', 'port-forward',
                                         'service/platform-gateway', f'{self.port}:8080'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        wait(lambda: self.healthy(), 'gateway port-forward', 60, 1)

    def healthy(self):
        try:
            return urllib.request.urlopen(self.url + '/healthz', timeout=3).status == 200
        except OSError:
            return False

    def close(self):
        self.process.terminate()


def http(url, tokens, team, method, path, payload=None):
    token = json.loads(Path(tokens).read_text())[team]
    request = urllib.request.Request(url + path, method=method, data=json.dumps(payload).encode() if payload is not None else None,
                                     headers={'Authorization': 'Bearer ' + token, 'Content-Type': 'application/json'})
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            return response.status, json.loads(response.read() or b'null')
    except urllib.error.HTTPError as e:
        return e.code, None


def kubectl_for(kubeconfig):
    def kubectl(*args, check=True, input=None):
        return run([tool('kubectl'), '--kubeconfig', kubeconfig, *args], check=check, input=input, quiet=True)
    return kubectl


def container_exists(name):
    return run(['docker', 'container', 'inspect', name], check=False, quiet=True).returncode == 0


def cluster_exists(name):
    clusters = json.loads(run([tool('k3d'), 'cluster', 'list', '-o', 'json'], quiet=True).stdout)
    return name in [c['name'] for c in clusters]


class Lifecycle:
    """Create, update, failure, retry and destroy through the released CLI and the gateway API."""

    def __init__(self, release, url, tokens, kubeconfig, team='team-a'):
        self.release, self.url, self.tokens, self.team = release, url, str(tokens), team
        self.kubeconfig, self.kubectl = kubeconfig, kubectl_for(kubeconfig)

    def cli(self, *args, check=True, timeout=1800):
        # connect and doctor read the cluster directly, with the installation's own kubeconfig.
        return self.release.panko('--url', self.url, '--tokens-file', self.tokens, '--kubeconfig', self.kubeconfig, '--team', self.team,
                                  *args, check=check, timeout=timeout, quiet=True)

    def status(self, name):
        result = self.cli('get', name, '--json', check=False)
        return json.loads(result.stdout) if result.returncode == 0 else None

    def resource(self, name):
        result = self.kubectl('get', 'databases.platform.example.com', name, '-n', self.team, '-o', 'json', check=False)
        return json.loads(result.stdout) if result.returncode == 0 else None

    def connection(self, name):
        """A digest of the connection Secret's data: compared, never printed."""
        result = self.kubectl('get', 'secret', name + '-connection', '-n', self.team, '-o', 'json', check=False)
        return digest(json.dumps(json.loads(result.stdout)['data'], sort_keys=True)) if result.returncode == 0 else None

    def state(self, name):
        result = self.kubectl('get', 'secret', f'tfstate-default-{self.team}-{name}', '-n', 'panko-state-' + self.team, '-o', 'json', check=False)
        return digest(json.dumps(json.loads(result.stdout)['data'], sort_keys=True)) if result.returncode == 0 else None

    def ready(self, name, generation=None, revision=None, timeout=900):
        def check():
            cr = self.resource(name)
            status = (cr or {}).get('status', {})
            if status.get('phase') == 'Failed' and (generation is None or status.get('observedGeneration') == generation):
                raise AssertionError(f'{name} failed: {status.get("message")}')
            return cr if status.get('phase') == 'Ready' and (generation is None or status.get('observedGeneration') == generation) \
                and (revision is None or status.get('observedRevision') == revision) else None
        return wait(check, f'{self.team}/{name} Ready', timeout)

    def create(self, name):
        self.cli('create', name, '--extensions', 'pgcrypto')
        self.cli('wait', name, '--timeout', '15m', timeout=1000)
        cr = self.ready(name)
        assert self.connection(name) and self.state(name), 'connection Secret or state missing'
        connect = json.loads(self.cli('connect', name, '--format', 'json').stdout)
        assert {'host', 'database', 'username', 'password'} <= connect.keys()
        log(f'ok created {self.team}/{name} (uid {cr["metadata"]["uid"]}), connection Secret and state present')
        return cr

    def update(self, name, limit):
        code, _ = http(self.url, self.tokens, self.team, 'PATCH', f'/v1/teams/{self.team}/databases/{name}', {'spec': {'connectionLimit': limit}})
        assert code == 200, code
        generation = self.resource(name)['metadata']['generation']
        cr = self.ready(name, generation=generation)
        assert cr['spec']['connectionLimit'] == limit
        log(f'ok updated {self.team}/{name} to generation {generation}')
        return cr

    def failure_and_retry(self, name, set_password):
        """A wrong PostgreSQL admin password fails the apply with code provider; retry repairs it."""
        set_password('not-the-password')
        try:
            self.cli('create', name, '--extensions', 'pgcrypto')
            failed = wait(lambda: (lambda s: s if s.get('phase') == 'Failed' else None)((self.resource(name) or {}).get('status', {})),
                          f'{name} Failed', 600)
            assert failed['lastExecution']['code'] == 'provider', failed['lastExecution']
        finally:
            set_password(None)
        self.cli('retry', name, '--wait', timeout=1000)
        cr = self.ready(name)
        assert cr['status']['lastExecution']['attempt'] >= 2, cr['status']['lastExecution']
        log(f'ok {self.team}/{name} failed with code provider and the explicit retry succeeded')

    def destroy(self, name):
        self.cli('delete', name, '--wait', '--timeout', '15m', timeout=1000)
        wait(lambda: self.resource(name) is None, f'{name} deleted', 600)
        assert self.connection(name) is None and self.state(name) is None, 'connection Secret or state left behind'
        log(f'ok destroyed {self.team}/{name}; connection Secret and state removed')


def postgres_password(kubectl):
    """Sets a wrong admin password, or restores the original one (value None)."""
    original = json.loads(kubectl('get', 'secret', 'postgres-admin', '-n', 'infra-backends', '-o', 'json').stdout)['data'][POSTGRES_PATCH_KEY]

    def set_password(value):
        data = base64.b64encode(value.encode()).decode() if value else original
        kubectl('patch', 'secret', 'postgres-admin', '-n', 'infra-backends', '--type=merge', '-p', json.dumps({'data': {POSTGRES_PATCH_KEY: data}}))
    return set_password


def kubeconfig_fingerprint():
    path = Path.home() / '.kube' / 'config'
    return digest(path.read_bytes()) if path.exists() else None


# --- mirror -----------------------------------------------------------------------------------

def phase_mirror(release):
    """The module mirror serves only once the pinned revision is in its clone, and a runner's fetch
    from a mirror that is still preparing succeeds through git-sync's retries."""
    mirror = release.image('panko-module-mirror')
    if release.images != 'docker':
        run(['docker', 'pull', '-q', mirror], quiet=True)
    git_sync = run([tool('helm'), 'show', 'values', release.chart], quiet=True).stdout.split('gitSync: ')[1].split()[0]
    network = 'panko-acc-mirror'
    # The pod's emptyDir volumes, writable for the mirror's user.
    volumes = ['--tmpfs', '/repositories:uid=1000,gid=1000', '--tmpfs', '/tmp:uid=1000,gid=1000']
    containers = ('panko-acc-mirror-missing', 'panko-acc-mirror', 'panko-acc-fetch')
    assert not any(container_exists(c) for c in containers), 'acceptance containers from another run exist'
    run(['docker', 'network', 'create', network], quiet=True)
    try:
        missing = '0' * 40
        run(['docker', 'run', '-d', '--name', 'panko-acc-mirror-missing', '--network', network, *volumes, '-e', 'MODULE_REPO=' + MODULE,
             '-e', 'MODULE_REVISION=' + missing, mirror], quiet=True)
        time.sleep(25)
        probe = run(['docker', 'exec', 'panko-acc-mirror-missing', 'git', 'ls-remote', 'git://127.0.0.1:9418/module.git', 'HEAD'], check=False, quiet=True)
        assert probe.returncode != 0, 'the mirror served without the pinned revision'
        logs = run(['docker', 'logs', 'panko-acc-mirror-missing'], quiet=True)
        assert 'is not in' in logs.stderr + logs.stdout, 'the mirror did not report the missing revision'
        log('ok the mirror does not serve while the pinned revision is missing')
        # The fetch starts before the mirror exists and succeeds once it serves the revision.
        fetch = subprocess.Popen(['docker', 'run', '--rm', '--name', 'panko-acc-fetch', '--network', network, git_sync,
                                  '--repo=git://panko-acc-mirror:9418/module.git', '--ref=' + MODULE_REVISION, '--root=/tmp/module',
                                  '--link=current', '--one-time', '--depth=1', '--submodules=off', '--max-failures=18', '--period=5s'],
                                 stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        time.sleep(10)
        run(['docker', 'run', '-d', '--name', 'panko-acc-mirror', '--network', network, *volumes, '-e', 'MODULE_REPO=' + MODULE,
             '-e', 'MODULE_REVISION=' + MODULE_REVISION, mirror], quiet=True)
        assert fetch.wait(timeout=180) == 0, 'the runner fetch did not survive the mirror start'
        log('ok a runner fetch that started before the mirror was ready succeeded through its retries')
    finally:
        for name in containers:
            run(['docker', 'rm', '-f', name], check=False, quiet=True)
        run(['docker', 'network', 'rm', network], check=False, quiet=True)


# --- server -----------------------------------------------------------------------------------

def phase_server(release):
    """The panko-server archive for remote agents (binary and systemd unit) and the agent image."""
    arch = run(['docker', 'version', '--format', '{{.Server.Arch}}'], quiet=True).stdout.strip()
    target = release.work / 'panko-server'
    target.mkdir()
    release.extract(f'panko-server_{release.version}_linux_{arch}.tar.gz', target)
    for name in ('panko-server', 'panko-agent.service', 'agent.env.example', 'profile.env.example', 'LICENSE'):
        assert (target / name).exists(), f'{name} missing from the panko-server archive'
    # A plain Debian with systemd: the unit verifies against the installed binary and user.
    script = ('apt-get update -qq > /dev/null && apt-get install -y -qq systemd > /dev/null 2>&1 && '
              'install -m 0755 /archive/panko-server /usr/local/bin/panko-server && useradd --system panko-agent && '
              'panko-server --version && systemd-analyze verify --man=no /archive/panko-agent.service')
    output = run(['docker', 'run', '--rm', '-v', f'{target}:/archive:ro', 'debian:13-slim', 'sh', '-c', script], quiet=True).stdout
    assert f'panko-server v{release.version}' in output, output
    log(f'ok panko-server archive for linux/{arch}: binary reports v{release.version}, systemd unit verifies')
    agent = release.image('panko-agent')
    if release.images != 'docker':
        run(['docker', 'pull', '-q', agent], quiet=True)
    versions = run(['docker', 'run', '--rm', '--entrypoint', 'sh', agent, '-c', 'panko-server --version && tofu version && git --version'], quiet=True).stdout
    chart_engine = run([tool('helm'), 'show', 'values', release.chart], quiet=True).stdout.split('engine: ghcr.io/opentofu/opentofu:')[1].split('@')[0]
    assert f'panko-server v{release.version}' in versions and f'OpenTofu v{chart_engine}' in versions, versions
    log(f'ok agent image: panko-server v{release.version}, OpenTofu {chart_engine} like the chart\'s engine, git')


# --- dev --------------------------------------------------------------------------------------

def phase_dev(release):
    name, port = 'panko-acc-dev', free_port()
    # Only a lab this run creates is ever deleted.
    assert not cluster_exists(name), f'k3d cluster {name} already exists; remove it or rerun later'
    flags = ['--name', name, '--port', str(port)]
    images = []
    if release.images == 'docker':
        images = ['--server-image', release.image('panko-server'), '--module-mirror-image', release.image('panko-module-mirror')]
    home = Path(release.env['PANKO_HOME'])
    lab = home / 'dev' / name
    try:
        # No k3d on PATH: refused without consent, then installed privately with its checksum.
        refused = release.panko('dev', 'up', *flags, *images, check=False)
        assert refused.returncode != 0 and 'install-k3d' in refused.stdout + refused.stderr, refused.stderr
        release.panko('dev', 'up', *flags, *images, '--install-k3d', '--yes', timeout=2400)
        status = release.panko('dev', 'status', '--name', name).stdout
        tokens, kubeconfig = lab / 'team-tokens.json', lab / 'kubeconfig'
        assert tokens.stat().st_mode & 0o077 == 0 and kubeconfig.stat().st_mode & 0o077 == 0, 'lab files are not private'
        assert json.loads(tokens.read_text())['team-a'] not in status, 'dev status printed a token'

        kubectl = kubectl_for(kubeconfig)
        url = f'http://127.0.0.1:{port}'
        life = Lifecycle(release, url, tokens, kubeconfig)
        life.cli('doctor')
        cr = life.create('orders')
        life.update('orders', 27)
        life.failure_and_retry('orders-retry', postgres_password(kubectl))
        before = (cr['metadata']['uid'], life.connection('orders'), life.state('orders'), digest(tokens.read_bytes()))
        # Restart: stop the lab's cluster, then dev up again starts and upgrades it in place.
        k3d = next((home / 'bin').glob('k3d-*'))
        run([k3d, 'cluster', 'stop', name], env={**release.env, 'KUBECONFIG': str(home / 'k3d-kubeconfig')}, quiet=True)
        release.panko('dev', 'up', *flags, *images, '--yes', timeout=2400)
        after = life.ready('orders')
        assert (after['metadata']['uid'], life.connection('orders'), life.state('orders'), digest(tokens.read_bytes())) == before, \
            'resource, credentials, state or tokens changed across the restart'
        log('ok restarted the lab; resource, connection Secret, state and tokens unchanged')
        life.destroy('orders')
        life.destroy('orders-retry')
        refused = release.panko('dev', 'down', '--name', name, input='no\n', check=False)
        assert refused.returncode != 0 and kubectl('get', 'namespace', 'team-a', check=False).returncode == 0, 'dev down without confirmation'
        release.panko('dev', 'down', '--name', name, input=name + '\n')
        assert name not in run([k3d, 'cluster', 'list'], env={**release.env, 'KUBECONFIG': str(home / 'k3d-kubeconfig')}, quiet=True).stdout
        log('ok dev down removed only this lab')
    except BaseException:
        k3d = next((home / 'bin').glob('k3d-*'), None)
        if k3d:
            run([k3d, 'cluster', 'delete', name], env={**release.env, 'KUBECONFIG': str(home / 'k3d-kubeconfig')}, check=False, quiet=True)
        raise


# --- install ----------------------------------------------------------------------------------

class PrivateRegistry:
    """A password-protected TLS registry that the cluster reaches only with pull secrets."""

    name = 'panko-acc-registry'

    def __init__(self, release):
        self.release, self.dir = release, release.work / 'registry'
        self.dir.mkdir()
        self.host = f'{self.name}:5000'
        self.password = base64.b32encode(os.urandom(15)).decode()
        run(['openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '2', '-subj', '/CN=' + self.name,
             '-addext', 'subjectAltName=DNS:' + self.name, '-keyout', self.dir / 'tls.key', '-out', self.dir / 'tls.crt'], quiet=True)
        htpasswd = run(['docker', 'run', '--rm', '--entrypoint', 'htpasswd', 'httpd:2.4-alpine', '-Bbn', 'panko', self.password], quiet=True).stdout
        (self.dir / 'htpasswd').write_text(htpasswd)
        (self.dir / 'registries.yaml').write_text(f'configs:\n  "{self.host}":\n    tls:\n      ca_file: /etc/panko-acc/tls.crt\n')
        for path in self.dir.iterdir():
            path.chmod(0o644)

    def cluster_args(self):
        return ['--registry-config', str(self.dir / 'registries.yaml'), '--volume', f'{self.dir / "tls.crt"}:/etc/panko-acc/tls.crt@server:0']

    def start(self, network):
        assert not container_exists(self.name), f'container {self.name} already exists'
        self.started = True
        run(['docker', 'run', '-d', '--name', self.name, '--network', network, '-v', f'{self.dir}:/certs:ro',
             '-e', 'REGISTRY_HTTP_TLS_CERTIFICATE=/certs/tls.crt', '-e', 'REGISTRY_HTTP_TLS_KEY=/certs/tls.key',
             '-e', 'REGISTRY_AUTH=htpasswd', '-e', 'REGISTRY_AUTH_HTPASSWD_REALM=panko', '-e', 'REGISTRY_AUTH_HTPASSWD_PATH=/certs/htpasswd',
             REGISTRY_IMAGE], quiet=True)
        for image in ('panko-server', 'panko-module-mirror'):
            source = self.release.image(image)
            target = f'{self.host}/hingst-it/{image}:{self.release.version}'
            script = f'crane auth login {self.host} -u panko --password-stdin --insecure && '
            mounts = []
            if self.release.images == 'docker':
                tar = self.release.work / f'{image}.tar'
                run(['docker', 'save', '-o', tar, source], quiet=True)
                mounts = ['-v', f'{tar}:/image.tar:ro']
                script += f'crane push --insecure /image.tar {target}'
            else:
                script += f'crane copy --insecure {source} {target}'
            run(['docker', 'run', '--rm', '-i', '--network', network, *mounts, '--entrypoint', 'sh', CRANE, '-c', script],
                input=self.password + '\n', quiet=True)
        anonymous = run(['docker', 'run', '--rm', '--network', network, '--entrypoint', 'sh', CRANE, '-c',
                         f'crane manifest --insecure {self.host}/hingst-it/panko-server:{self.release.version}'], check=False, quiet=True)
        assert anonymous.returncode != 0, 'the private registry allowed an anonymous pull'
        log('ok private registry refuses anonymous pulls; images copied with credentials')

    def pull_secret(self, cluster, namespaces):
        auth = base64.b64encode(f'panko:{self.password}'.encode()).decode()
        config = json.dumps({'auths': {self.host: {'auth': auth}}})
        for namespace in namespaces:
            cluster.kubectl('create', 'namespace', namespace, check=False)
            secret = {'apiVersion': 'v1', 'kind': 'Secret', 'type': 'kubernetes.io/dockerconfigjson',
                      'metadata': {'name': 'panko-registry', 'namespace': namespace},
                      'data': {'.dockerconfigjson': base64.b64encode(config.encode()).decode()}}
            cluster.kubectl('apply', '-f', '-', input=json.dumps(secret))

    def stop(self):
        if getattr(self, 'started', False):
            run(['docker', 'rm', '-f', self.name], check=False, quiet=True)


def retained(cluster, database):
    """What helm uninstall and GitOps pruning must keep: data, credentials, CRDs and definitions."""
    for kind, name, namespace in [('namespace', 'infra-runners', None), ('namespace', 'team-a', None), ('namespace', 'panko-state-team-a', None),
                                  ('crd', 'databases.platform.example.com', None), ('crd', 'compositecontrollers.metacontroller.k8s.io', None),
                                  ('compositecontroller', 'database-controller', None), ('statefulset', 'postgres', 'infra-backends'),
                                  ('pvc', 'data-postgres-0', 'infra-backends'), ('secret', 'gateway-auth', 'infra-gateway'),
                                  ('secret', 'postgres-admin', 'infra-backends')]:
        assert cluster.get(kind, name, namespace), f'{kind} {name} was not retained'
    cr = cluster.get('databases.platform.example.com', database, 'team-a')
    assert cr and cr['metadata'].get('finalizers'), 'the Database or its destroy finalizer was not retained'
    assert cluster.get('deployment', 'database-hook', 'infra-runners') is None, 'the controller still runs'


def pem_certificates(data):
    """Number of certificates in a base64 PEM bundle from a Secret."""
    return base64.b64decode(data).decode().count('BEGIN CERTIFICATE')


def served_by(kubeconfig, service, port, ca_data, hostname, timeout=180):
    """Waits until the Service presents a certificate that verifies against ca_data alone."""
    import ssl
    local = free_port()
    forward = subprocess.Popen([tool('kubectl'), '--kubeconfig', kubeconfig, '-n', 'infra-runners', 'port-forward', 'service/' + service,
                                f'{local}:{port}'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        context = ssl.create_default_context(cadata=base64.b64decode(ca_data).decode())

        def handshake():
            try:
                with socket.create_connection(('127.0.0.1', local), timeout=5) as raw:
                    with context.wrap_socket(raw, server_hostname=hostname):
                        return True
            except OSError:
                return False
        wait(handshake, f'{service} serving a certificate of the new CA', timeout, 5)
    finally:
        forward.terminate()


def ca_rollover(release, cluster, install):
    """docs/operations.md#replacing-a-ca-manual-trust-rollover for both CAs."""
    for secret, service, port in [('provider-mirror-tls', 'provider-mirror', 8443), ('runner-endpoint-tls', 'runner-endpoint', 443)]:
        old = cluster.get('secret', secret, 'infra-runners')['data']
        cluster.kubectl('delete', 'secret', secret + '-ca', '-n', 'infra-runners')
        release.panko(*install, timeout=1800, quiet=True)
        staged = cluster.get('secret', secret, 'infra-runners')['data']
        assert staged['tls.crt'] == old['tls.crt'] and pem_certificates(staged['ca.crt']) == 2, f'{secret}: no staged old+new bundle'
        cluster.kubectl('delete', 'secret', secret, '-n', 'infra-runners')
        release.panko(*install, timeout=1800, quiet=True)
        new = cluster.get('secret', secret, 'infra-runners')['data']
        assert new['tls.crt'] != old['tls.crt'] and pem_certificates(new['ca.crt']) == 1, f'{secret}: not reissued from the new CA'
        assert 'ca.key' not in new, 'the CA key reached the serving Secret'
        served_by(cluster.kubeconfig, service, port, new['ca.crt'], f'{service}.infra-runners.svc')
        log(f'ok CA rollover of {secret}: staged bundle, reissued certificate served without restart')


def full_cleanup(cluster, teams):
    """docs/operations.md#uninstalling-and-cleaning-up-retained-objects, on an installation without resources."""
    assert not json.loads(cluster.kubectl('get', 'databases.platform.example.com', '-A', '-o', 'json').stdout)['items']
    run([tool('helm'), '--kubeconfig', cluster.kubeconfig, 'uninstall', 'panko', '--namespace', 'panko-system', '--wait'], quiet=True)
    cluster.kubectl('delete', 'compositecontroller', 'database-controller')
    cluster.kubectl('delete', 'clusterrole,clusterrolebinding', '-l', 'app.kubernetes.io/part-of=panko')
    namespaces = ['infra-runners', 'infra-gateway', 'infra-backends', 'panko-system'] + [n for t in teams for n in (t, 'panko-state-' + t)]
    cluster.kubectl('delete', 'namespace', *namespaces, '--wait=true', '--timeout=600s')
    cluster.kubectl('delete', 'crd', 'databases.platform.example.com', 'runs.platform.example.com', 'statebackends.platform.example.com',
                    'backendprofiles.platform.example.com', 'compositecontrollers.metacontroller.k8s.io',
                    'controllerrevisions.metacontroller.k8s.io', 'decoratorcontrollers.metacontroller.k8s.io')
    left = cluster.kubectl('get', 'all,clusterrole,clusterrolebinding,crd,namespace,pvc,pv', '-A', '-o', 'name', '-l', 'app.kubernetes.io/part-of=panko').stdout
    leftover = [line for line in left.splitlines() if line.strip()]
    crds = [line for line in cluster.kubectl('get', 'crd', '-o', 'name').stdout.splitlines() if 'platform.example.com' in line or 'metacontroller' in line]
    assert not leftover and not crds, leftover + crds
    log('ok the documented cleanup removed every Panko object')


def phase_install(release):
    registry = PrivateRegistry(release)
    cluster = Cluster(release, 'panko-acc-install', *registry.cluster_args())
    gateway = None
    try:
        registry.start('k3d-' + cluster.name)
        node = f'k3d-{cluster.name}-server-0'
        registry.pull_secret(cluster, ['infra-runners', 'infra-gateway', 'infra-backends', 'team-a', 'team-b'])
        values = release.work / 'install-values.yaml'
        values.write_text(json.dumps({
            'images': {'server': {'repository': f'{registry.host}/hingst-it/panko-server'},
                       'moduleMirror': {'repository': f'{registry.host}/hingst-it/panko-module-mirror'},
                       'pullSecrets': ['panko-registry']},
            'module': {'revision': OLD_MODULE},
            'installation': {'s3': True, 'runnerEndpoint': {'names': [node]}},
            'endpoint': {'labNodePort': 30443}}))
        # 1. Helm directly, from the packaged chart; the namespaces with the pull secrets are adopted.
        run([tool('helm'), '--kubeconfig', cluster.kubeconfig, 'install', 'panko', release.chart, '--namespace', 'panko-system',
             '--create-namespace', '--take-ownership', '--values', values, '--wait', '--timeout', '15m'], quiet=True, timeout=1200)
        hook = cluster.get('deployment', 'database-hook', 'infra-runners')
        assert hook['spec']['template']['spec']['containers'][0]['image'].startswith(registry.host), 'not pulled from the private registry'
        cluster.kubectl('apply', '-f', '-', input=json.dumps({'apiVersion': 'platform.example.com/v1', 'kind': 'Database',
                        'metadata': {'name': 'helm-direct', 'namespace': 'team-a'}, 'spec': {'owner': 'team-a', 'extensions': ['pgcrypto']}}))
        life = Lifecycle(release, None, None, cluster.kubeconfig)
        life.ready('helm-direct', revision=OLD_MODULE)
        jobs = json.loads(cluster.kubectl('get', 'jobs', '-n', 'team-a', '-o', 'json').stdout)['items']
        assert jobs and all(j['spec']['template']['spec'].get('serviceAccountName') == 'tofu-runner' for j in jobs)
        log('ok helm install from the packaged chart; a runner Job pulled panko-server through the pull secret')
        # 2. panko install takes over the same release, twice; tokens stay the same.
        tokens = release.work / 'install-tokens.json'
        install = ['install', '--kubeconfig', cluster.kubeconfig, '--values', values, '--tokens-file', tokens, '--yes']
        release.panko(*install, timeout=1800, quiet=True)
        first = digest(tokens.read_bytes())
        release.panko(*install, timeout=1800, quiet=True)
        assert digest(tokens.read_bytes()) == first, 'a repeated install changed the team tokens'
        log('ok panko install upgraded the Helm release twice; tokens unchanged')
        gateway = Gateway(cluster.kubeconfig, tokens)
        life = Lifecycle(release, gateway.url, tokens, cluster.kubeconfig)
        life.create('orders')
        # 3. Module upgrade: a new pinned revision is applied without module_fetch failures.
        release.panko(*install, '--set', 'module.revision=' + MODULE_REVISION, timeout=1800, quiet=True)
        life.update('orders', 28)
        for name in ('orders', 'helm-direct'):
            cr = life.ready(name, revision=MODULE_REVISION)
            assert cr['status']['lastExecution'].get('code') in (None, ''), cr['status']['lastExecution']
        log('ok module upgrade applied the new revision to every resource without fetch failures')
        life.failure_and_retry('orders-retry', postgres_password(cluster.kubectl))
        # 4. The packaged remote agent, with the released CLI as administrator command.
        agent = release.image('panko-agent')
        if release.images != 'docker':
            run(['docker', 'pull', '-q', agent], quiet=True)
        mirror = release.image('panko-module-mirror')
        if release.images != 'docker':
            run(['docker', 'pull', '-q', mirror], quiet=True)
        env = {**os.environ, 'KUBECONFIG': str(cluster.kubeconfig), 'CLUSTER_NAME': cluster.name, 'PANKO_TOKENS_FILE': str(tokens),
               'PANKO_ADMIN': f'{release.bin / "panko"} admin', 'PANKO_AGENT_IMAGE': agent, 'PANKO_MODULE_MIRROR_IMAGE': mirror,
               'PANKO_CONFIG': str(release.work / 'agent-config.json'), 'PATH': f'{Path(tool("kubectl")).parent}:{os.environ["PATH"]}'}
        (release.work / 'agent-config.json').write_text(json.dumps({'teams': ['team-a', 'team-b'], 's3': True}))
        run([ROOT / '.venv/bin/python' if (ROOT / '.venv/bin/python').exists() else 'python3', 'test/remote_e2e.py'], env=env, cwd=ROOT, timeout=2400)
        log('ok remote runner pool with the packaged agent image')
        # The agent test changed the pool configuration; the next install restores the chart's.
        install += ['--set', 'module.revision=' + MODULE_REVISION]
        release.panko(*install, timeout=1800, quiet=True)
        # 5. Manual CA rollover for both CAs, then an apply through the renewed provider mirror.
        ca_rollover(release, cluster, install)
        life.update('orders', 30)
        # 6. helm uninstall keeps data, credentials, CRDs and the Databases with their finalizers.
        gateway.close()
        gateway = None
        run([tool('helm'), '--kubeconfig', cluster.kubeconfig, 'uninstall', 'panko', '--namespace', 'panko-system', '--wait'], quiet=True)
        retained(cluster, 'orders')
        log('ok helm uninstall retained namespaces, CRDs, PostgreSQL, Secrets and the Databases with finalizers')
        # 7. Reinstall continues managing the same resources.
        release.panko(*install, timeout=1800, quiet=True)
        assert digest(tokens.read_bytes()) == first, 'reinstalling changed the team tokens'
        gateway = Gateway(cluster.kubeconfig, tokens)
        life = Lifecycle(release, gateway.url, tokens, cluster.kubeconfig)
        life.update('orders', 29)
        for name in ('orders', 'orders-retry', 'helm-direct'):
            life.destroy(name)
        log('ok reinstall resumed management; update and destroy succeeded')
        gateway.close()
        gateway = None
        # 8. Complete removal of an empty installation.
        full_cleanup(cluster, ['team-a', 'team-b'])
    finally:
        if gateway:
            gateway.close()
        cluster.delete()
        registry.stop()


# --- gitops -----------------------------------------------------------------------------------

LAB_VALUES = {'installation': {'teams': ['team-a', 'team-b']}}


def lifecycle_with_kubectl(cluster, name):
    cluster.kubectl('apply', '-f', '-', input=json.dumps({'apiVersion': 'platform.example.com/v1', 'kind': 'Database',
                    'metadata': {'name': name, 'namespace': 'team-a'}, 'spec': {'owner': 'team-a', 'extensions': ['pgcrypto']}}))
    return Lifecycle(None, None, None, cluster.kubeconfig).ready(name)


def pinned(release, path, key):
    """An example pinned to the release's stable version, applied with the tested version."""
    text = (ROOT / path).read_text()
    stable = release.version.split('-')[0]
    assert f'{key}{stable}\n' in text, f'{path} is not pinned to {stable}'
    return text.replace(f'{key}{stable}\n', f'{key}{release.version}\n')


def phase_argocd(release):
    cluster = Cluster(release, 'panko-acc-argocd')
    try:
        cluster.kubectl('create', 'namespace', 'argocd')
        cluster.kubectl('apply', '--server-side', '-n', 'argocd', '-f', f'https://raw.githubusercontent.com/argoproj/argo-cd/{ARGOCD}/manifests/install.yaml')
        cluster.kubectl('-n', 'argocd', 'rollout', 'status', 'statefulset/argocd-application-controller', '--timeout=300s')
        cluster.kubectl('-n', 'argocd', 'rollout', 'status', 'deployment/argocd-repo-server', '--timeout=300s')
        # The example as published, with the tested version and the values inline instead of
        # its second source (your own values repository).
        app = yaml.safe_load(pinned(release, 'examples/gitops/argocd/application.yaml', 'targetRevision: '))
        chart_source = app['spec'].pop('sources')[0]
        chart_source['helm'] = {'releaseName': 'panko', 'valuesObject': LAB_VALUES}
        app['spec']['source'] = chart_source
        cluster.kubectl('apply', '-f', '-', input=json.dumps(app))

        def healthy():
            status = (cluster.get('application', 'panko', 'argocd') or {}).get('status', {})
            return status.get('sync', {}).get('status') == 'Synced' and status.get('health', {}).get('status') == 'Healthy'
        wait(healthy, 'Argo CD Application Synced and Healthy', 1200, 10)
        lifecycle_with_kubectl(cluster, 'argo-orders')
        log('ok Argo CD installed the OCI chart; a Database became Ready')
        cluster.kubectl('delete', 'application', 'panko', '-n', 'argocd', '--wait=true', '--timeout=600s')
        retained(cluster, 'argo-orders')
        log('ok deleting the Argo CD Application retained data, CRDs and the Database')
    finally:
        cluster.delete()


def phase_flux(release):
    cluster = Cluster(release, 'panko-acc-flux')
    try:
        cluster.kubectl('apply', '-f', f'https://github.com/fluxcd/flux2/releases/download/{FLUX}/install.yaml')
        for name in ('source-controller', 'helm-controller'):
            cluster.kubectl('-n', 'flux-system', 'rollout', 'status', 'deployment/' + name, '--timeout=300s')
        cluster.kubectl('create', 'configmap', 'panko-values', '-n', 'flux-system', '--from-literal=values.yaml=' + json.dumps(LAB_VALUES))
        cluster.kubectl('apply', '-f', '-', input=pinned(release, 'examples/gitops/flux/panko.yaml', 'tag: '))

        def ready():
            conditions = (cluster.get('helmrelease', 'panko', 'flux-system') or {}).get('status', {}).get('conditions', [])
            return any(c['type'] == 'Ready' and c['status'] == 'True' for c in conditions)
        wait(ready, 'Flux HelmRelease Ready', 1200, 10)
        lifecycle_with_kubectl(cluster, 'flux-orders')
        log('ok Flux installed the OCI chart; a Database became Ready')
        cluster.kubectl('delete', 'helmrelease', 'panko', '-n', 'flux-system', '--wait=true', '--timeout=600s')
        retained(cluster, 'flux-orders')
        log('ok deleting the Flux HelmRelease retained data, CRDs and the Database')
    finally:
        cluster.delete()


def phase_gitops(release):
    phase_argocd(release)
    phase_flux(release)


# --- migrate ----------------------------------------------------------------------------------

def phase_migrate(release):
    """docs/upgrading.md on a v0.0.1 installation made from its tag, as v0.0.1 documented it."""
    source = release.work / 'v0.0.1'
    name = 'panko-acc-migrate'
    # v0.0.1's cluster-up would reuse an existing cluster; only one this run creates is deleted.
    assert not cluster_exists(name), f'k3d cluster {name} already exists; remove it or rerun later'
    run(['git', '-C', ROOT, 'worktree', 'add', '--detach', source, 'v0.0.1'], quiet=True)
    lab = {**os.environ, 'CLUSTER_NAME': name}
    lab.pop('KUBECONFIG', None)
    try:
        run(['make', 'cluster-up', 'bootstrap', 'build', 'deploy'], env=lab, cwd=source, timeout=3600, quiet=True)
        kubeconfig = source / '.local/kubeconfig'

        kubectl = kubectl_for(kubeconfig)
        kubectl('apply', '-f', '-', input=json.dumps({'apiVersion': 'platform.example.com/v1', 'kind': 'Database',
                'metadata': {'name': 'orders', 'namespace': 'team-a'}, 'spec': {'owner': 'team-a', 'extensions': ['pgcrypto'], 'connectionLimit': 20}}))
        life = Lifecycle(release, None, None, kubeconfig)
        cr = life.ready('orders')

        def jobs():
            return len(json.loads(kubectl('get', 'jobs', '-n', 'team-a', '-o', 'json').stdout)['items'])
        before = {'uid': cr['metadata']['uid'], 'finalizers': cr['metadata']['finalizers'], 'hash': cr['status']['lastAppliedHash'],
                  'connection': life.connection('orders'), 'state': life.state('orders'), 'jobs': jobs(),
                  'tokens': digest((source / '.local/team-tokens.json').read_bytes())}
        assert kubectl('get', 'namespace', 'metacontroller', check=False).returncode == 0, 'not a v0.0.1 installation'
        log('ok v0.0.1 installation with a Ready Database')
        # docs/upgrading.md, step by step.
        backup = release.work / 'v0.0.1-backup'
        run(['make', 'backup', f'BACKUP_DIR={backup}'], env=lab, cwd=source, timeout=1200, quiet=True)
        config = source / 'panko.json'
        admin = ['admin', '--kubeconfig', kubeconfig, '--config', config]
        import_candidate_images(release, name)
        release.panko(*admin, 'pause', quiet=True)
        run([tool('helm'), '--kubeconfig', kubeconfig, 'uninstall', 'metacontroller', '--namespace', 'metacontroller', '--wait'], quiet=True)
        kubectl('delete', 'namespace', 'metacontroller', '--wait=true', '--timeout=300s')
        tokens = release.work / 'migrate-tokens.json'
        install = ['install', '--kubeconfig', kubeconfig, '--config', config, '--tokens-file', tokens, '--take-ownership', '--yes']
        release.panko(*install, timeout=1800, quiet=True)
        release.panko(*admin, 'resume', quiet=True)
        kubectl('-n', 'infra-backends', 'delete', 'job', 's3-onboard', '--ignore-not-found')
        time.sleep(30)
        cr = life.ready('orders')
        shortnames = json.loads(kubectl('get', 'crd', 'databases.platform.example.com', '-o', 'json').stdout)['spec']['names']
        after = {'uid': cr['metadata']['uid'], 'finalizers': cr['metadata']['finalizers'], 'hash': cr['status']['lastAppliedHash'],
                 'connection': life.connection('orders'), 'state': life.state('orders'), 'jobs': jobs(), 'tokens': digest(tokens.read_bytes())}
        assert after == before, {k: (before[k], after[k]) for k in before if before[k] != after[k]}
        assert 'shortNames' not in shortnames and shortnames.get('categories') == ['panko'], shortnames
        assert kubectl('get', 'namespace', 'metacontroller', check=False).returncode != 0
        log('ok migrated: identity, finalizers, state, connection Secret and tokens unchanged; no new apply; CRD names updated')
        # docs/operations.md#legacy-ca-migration: the valid v0.0.1 certificate is kept with an
        # old+new bundle; deleting it finishes the transition to the stable CA.
        mirror_tls = json.loads(kubectl('get', 'secret', 'provider-mirror-tls', '-n', 'infra-runners', '-o', 'json').stdout)['data']
        assert pem_certificates(mirror_tls['ca.crt']) == 2, 'no old+new CA bundle after the migration'
        kubectl('delete', 'secret', 'provider-mirror-tls', '-n', 'infra-runners')
        release.panko(*install, timeout=1800, quiet=True)
        renewed = json.loads(kubectl('get', 'secret', 'provider-mirror-tls', '-n', 'infra-runners', '-o', 'json').stdout)['data']
        assert pem_certificates(renewed['ca.crt']) == 1 and renewed['tls.crt'] != mirror_tls['tls.crt']
        served_by(kubeconfig, 'provider-mirror', 8443, renewed['ca.crt'], 'provider-mirror.infra-runners.svc')
        log('ok legacy CA migration: staged bundle, then a certificate from the stable CA')
        gateway = Gateway(kubeconfig, tokens)
        try:
            life = Lifecycle(release, gateway.url, tokens, kubeconfig)
            life.update('orders', 26)
            life.destroy('orders')
        finally:
            gateway.close()
        log('ok after the migration an update and a complete destroy succeeded')
    finally:
        run([tool('k3d'), 'cluster', 'delete', name], check=False, quiet=True)
        run(['git', '-C', ROOT, 'worktree', 'remove', '--force', source], check=False, quiet=True)


PHASES = {'server': phase_server, 'mirror': phase_mirror, 'dev': phase_dev, 'install': phase_install, 'gitops': phase_gitops, 'migrate': phase_migrate}


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--dist', required=True)
    parser.add_argument('--version', required=True)
    parser.add_argument('--images', default=REGISTRY, choices=[REGISTRY, 'docker'])
    parser.add_argument('--work', help='work directory (default: a new private directory under .local)')
    parser.add_argument('phases', nargs='*', default=['server', 'mirror', 'dev', 'install', 'migrate'])
    args = parser.parse_args()
    unknown = set(args.phases) - PHASES.keys()
    if unknown:
        parser.error('unknown phases: ' + ', '.join(sorted(unknown)))
    (ROOT / '.local').mkdir(exist_ok=True)
    work = Path(args.work or tempfile.mkdtemp(prefix='acceptance-', dir=ROOT / '.local')).resolve()
    work.mkdir(parents=True, exist_ok=True)
    work.chmod(0o700)
    (work / 'empty').mkdir()
    release = Release(args.dist, args.version, args.images, work)
    global_kubeconfig = kubeconfig_fingerprint()
    failed = []
    for phase in args.phases:
        log(f'== {phase}')
        try:
            PHASES[phase](release)
            log(f'== {phase}: passed')
        except Exception:
            traceback.print_exc()
            failed.append(phase)
            log(f'== {phase}: FAILED')
    assert kubeconfig_fingerprint() == global_kubeconfig, 'the global kubeconfig changed'
    log('ok the global kubeconfig is unchanged')
    if failed:
        raise SystemExit('failed phases: ' + ', '.join(failed))
    log('All release acceptance phases passed: ' + ', '.join(args.phases))


if __name__ == '__main__':
    main()
