"""Remote runner pool against an emulated cloud (#26): an agent outside the cluster provisions
an S3 bucket in Moto, which only the agent can reach, through the gateway and the runner endpoint."""
import json
import os
import shutil
import subprocess
import tempfile
import traceback
from pathlib import Path

from e2e import K, RUN, get, kube, wait

CLUSTER = os.environ.get('CLUSTER_NAME', 'infra-lab')
CONFIG = os.environ.get('KUBECONFIG', '.local/kubeconfig')
MOTO = 'motoserver/moto:5.2.3@sha256:91fd602a21f49cf9eb82fdf474015a3c131d40104c8297ea6a2ca920708ae32c'
TEAM = 'team-b'
SUFFIX = RUN[-6:]
NETWORK = 'panko-cloud-' + SUFFIX
NAMES = {'moto': 'moto-' + SUFFIX, 'git': 'git-' + SUFFIX, 'agent': 'agent-' + SUFFIX, 'second': 'agent2-' + SUFFIX}
URL = 'http://127.0.0.1:18085'
TOKENS = json.loads(Path('.local/team-tokens.json').read_text())
PLUGIN_CACHE = Path('.local/agent-plugin-cache').resolve()


LOCK_PROBE = '''
import botocore
s3.put_object(Bucket='panko-state', Key='probe.tflock', Body=b'1', IfNoneMatch='*')
try:
    s3.put_object(Bucket='panko-state', Key='probe.tflock', Body=b'2', IfNoneMatch='*')
    print('accepted')
except botocore.exceptions.ClientError as e:
    print(e.response['Error']['Code'])
s3.delete_object(Bucket='panko-state', Key='probe.tflock')
'''


def docker(*args, check=True, timeout=600):
    # Every call is bounded, so a stalled daemon cannot outlast the waits that poll through it.
    result = subprocess.run(['docker', *args], capture_output=True, text=True, timeout=timeout)
    assert not check or result.returncode == 0, (args, result.stderr)
    return result


def admin(*args, config=None):
    flags = ['--config', config] if config else []
    result = subprocess.run(['.local/bin/admin', '--kubeconfig', CONFIG, *flags, *args], capture_output=True, text=True)
    assert result.returncode == 0, (args, result.stderr)
    return result.stdout


def curl(method, path, payload=None, expected=200):
    # The token goes through stdin, never argv.
    command = ['curl', '--silent', '--show-error', '--max-time', '25', '--config', '-', '-X', method, URL + path, '-w', '\n%{http_code}']
    if payload is not None:
        command += ['-H', 'Content-Type: application/json', '--data-binary', json.dumps(payload)]
    response = subprocess.run(command, input=f'header = "Authorization: Bearer {TOKENS[TEAM]}"\n', text=True, capture_output=True)
    assert response.returncode == 0, response.stderr
    text, code = response.stdout.rsplit('\n', 1)
    assert int(code) == expected, (method, path, code, text)
    return json.loads(text) if text else None


def moto(script):
    """Run boto3 inside the Moto container; the emulator is not reachable from anywhere else."""
    prelude = ("import boto3, json\n"
               "s3 = boto3.client('s3', endpoint_url='http://localhost:5000', aws_access_key_id='test', aws_secret_access_key='test', region_name='us-east-1')\n")
    return docker('exec', NAMES['moto'], 'python', '-c', prelude + script).stdout.strip()


def bucket(name):
    """Tags of the resource's bucket, or None while it does not exist."""
    return json.loads(moto(f"""
found = [b['Name'] for b in s3.list_buckets()['Buckets'] if b['Name'].startswith('panko-{name}-')]
print(json.dumps({{t['Key']: t['Value'] for t in s3.get_bucket_tagging(Bucket=found[0])['TagSet']}} if found else None))
"""))


def tamper(name, tags):
    moto(f"""
found = [b['Name'] for b in s3.list_buckets()['Buckets'] if b['Name'].startswith('panko-{name}-')]
s3.put_bucket_tagging(Bucket=found[0], Tagging={{'TagSet': [{{'Key': k, 'Value': v}} for k, v in {tags!r}.items()]}})
""")


def state_locks():
    return json.loads(moto("""
print(json.dumps([o['Key'] for o in s3.list_objects_v2(Bucket='panko-state').get('Contents', []) if o['Key'].endswith('.tflock')]))
"""))


def remove_state_locks():
    """The documented recovery step after an agent died mid-run: drop the S3 state lock it left."""
    return json.loads(moto("""
locks = [o['Key'] for o in s3.list_objects_v2(Bucket='panko-state').get('Contents', []) if o['Key'].endswith('.tflock')]
for key in locks:
    s3.delete_object(Bucket='panko-state', Key=key)
print(json.dumps(locks))
"""))


def status(name):
    b = get('bucket', name, TEAM)
    return b.get('status') or {} if b else {}


def attempt(name):
    return (status(name).get('lastExecution') or {}).get('attempt', 0)


def ready(name, previous=None, after=None):
    """Wait for Ready with a new applied hash, or after a newer attempt than `after`; only a failure
    of an attempt this wait is for counts, not the earlier one that is still in the status."""
    def check():
        s = status(name)
        if s.get('phase') == 'Failed' and (after is None or attempt(name) > after):
            raise AssertionError(s)
        if s.get('phase') == 'Ready' and s.get('lastAppliedHash') != previous and (after is None or attempt(name) > after):
            return s
        return None
    return wait(check, f'{name} Ready', 600)


def write_profile(directory, **overrides):
    values = {'AWS_ACCESS_KEY_ID': 'test', 'AWS_SECRET_ACCESS_KEY': 'test', 'TF_VAR_endpoint': 'http://moto:5000'}
    values.update(overrides)
    path = directory / 'profiles' / (TEAM + '.env')
    path.write_text(''.join(f'{k}={v}\n' for k, v in values.items() if v is not None))
    path.chmod(0o600)


def fixture_repository(root):
    """A module repository with only the test module, served to the agent by a git daemon."""
    work, bare = root / 'work', root / 'repos' / 'modules.git'
    shutil.copytree('test/modules/aws-s3', work / 'aws-s3')
    git = ['git', '-C', str(work), '-c', 'user.email=e2e@example.com', '-c', 'user.name=e2e']
    subprocess.run(['git', 'init', '--quiet', str(work)], check=True)
    subprocess.run(git + ['add', '.'], check=True)
    subprocess.run(git + ['commit', '--quiet', '-m', 'test module'], check=True)
    subprocess.run(['git', 'clone', '--quiet', '--bare', str(work), str(bare)], check=True)
    revision = subprocess.run(git + ['rev-parse', 'HEAD'], check=True, capture_output=True, text=True).stdout.strip()
    for path in [root / 'repos', *(root / 'repos').rglob('*')]:
        path.chmod(0o755 if path.is_dir() else 0o644)
    root.chmod(0o755)
    return revision


def start_agent(container, directory):
    node = 'k3d-' + CLUSTER + '-server-0'
    ip = docker('inspect', '-f', '{{(index .NetworkSettings.Networks "k3d-' + CLUSTER + '").IPAddress}}', node).stdout.strip()
    # The AWS provider is a 740 MB download (~30s); both agents and later local runs share one cache.
    # Only one agent executes at a time, and the module lockfile still verifies every provider.
    PLUGIN_CACHE.mkdir(parents=True, exist_ok=True)
    # The agent runs as the host user, so the 0600 token and profile it is given stay private.
    docker('run', '-d', '--name', container, '--network', 'k3d-' + CLUSTER, '--add-host', f'runner-endpoint.infra-runners.svc:{ip}',
           '--user', f'{os.getuid()}:{os.getgid()}', '-v', f'{directory}:/etc/panko-agent:ro', '-v', f'{PLUGIN_CACHE}:/var/cache/panko-plugins',
           '-e', 'PANKO_ENDPOINT=https://runner-endpoint.infra-runners.svc:30443', '-e', 'PANKO_ENDPOINT_CA=/etc/panko-agent/ca.crt',
           '-e', 'PANKO_AGENT_TOKEN_FILE=/etc/panko-agent/token', '-e', 'PANKO_POOL=cloud', '-e', 'PANKO_PROFILES=/etc/panko-agent/profiles',
           '-e', 'PANKO_WORK=/tmp', '-e', 'PANKO_PLUGIN_CACHE=/var/cache/panko-plugins', 'panko-agent:dev', 'run')
    docker('network', 'connect', '--alias', container, NETWORK, container)
    check = docker('exec', container, 'panko-server', 'agent', 'check', check=False)
    assert check.returncode == 0, check.stdout + check.stderr


def agent_directory(root, label, config_path):
    directory = root / label
    (directory / 'profiles').mkdir(parents=True)
    directory.chmod(0o700)
    (directory / 'profiles').chmod(0o700)
    (directory / 'token').write_text(admin('add-agent', '--pool', 'cloud', config=str(config_path)).strip())
    (directory / 'token').chmod(0o600)
    (directory / 'ca.crt').write_text(admin('endpoint-ca'))
    write_profile(directory)
    return directory


def setup(root):
    # Metacontroller discovers new CRDs on an interval; applying it first lets that overlap the setup.
    kube('apply', '-f', 'test/bucket-crd.yaml')
    kube('wait', '--for=condition=Established', 'crd/buckets.platform.example.com', '--timeout=60s')
    docker('network', 'create', NETWORK)
    docker('run', '-d', '--name', NAMES['moto'], '--network', NETWORK, '--network-alias', 'moto', '-e', 'S3_IGNORE_SUBDOMAIN_BUCKETNAME=true', MOTO)
    wait(lambda: docker('exec', NAMES['moto'], 'python', '-c', 'import urllib.request; urllib.request.urlopen("http://localhost:5000")', check=False).returncode == 0, 'Moto', 60)
    moto("s3.create_bucket(Bucket='panko-state')")
    # The S3 state lock relies on conditional writes; a second one must be refused.
    assert moto(LOCK_PROBE) == 'PreconditionFailed', 'Moto does not refuse a competing conditional write, so state locking is untested'
    docker('build', '-q', '-t', 'panko-agent:dev', '-f', 'Dockerfile.agent', '.')
    revision = fixture_repository(root)
    # The module mirror image carries git-daemon; the agent image only has the git client.
    docker('run', '-d', '--name', NAMES['git'], '--network', NETWORK, '--network-alias', 'git', '-v', f'{root / "repos"}:/repos:ro',
           '--entrypoint', 'git', 'panko-module-mirror:dev', '-c', 'safe.directory=*', 'daemon', '--base-path=/repos', '--export-all', '--reuseaddr', '/repos')
    # The pool, its module and its agents, configured exactly as an operator would.
    config = json.loads(Path(os.environ.get('PANKO_CONFIG', 'panko.json')).read_text())
    # A short lease lets the killed agent's Run become interrupted after ~25s instead of the default 60s.
    config['runnerPools'] = [{'name': 'cloud', 'teams': [TEAM], 'module': {'repository': 'git://git/modules.git', 'revision': revision, 'path': 'aws-s3'},
                              'leaseSeconds': 25, 'heartbeatSeconds': 3}]
    config_path = root / 'panko.json'
    config_path.write_text(json.dumps(config))
    kube('apply', '-f', '-', obj=json.loads(admin('runner-pools', config=str(config_path))))
    kube('-n', 'infra-runners', 'rollout', 'restart', 'deploy/database-hook')
    kube('-n', 'infra-runners', 'rollout', 'status', 'deploy/database-hook', '--timeout=120s')
    first = agent_directory(root, 'agent', config_path)
    start_agent(NAMES['agent'], first)
    # The team's S3 state backend lives in Moto as well; its credentials are in the agent's profile.
    kube('apply', '-f', '-', obj={'apiVersion': 'v1', 'kind': 'List', 'items': [
        {'apiVersion': 'platform.example.com/v1', 'kind': 'BackendProfile', 'metadata': {'name': 'moto', 'namespace': TEAM},
         'spec': {'type': 's3', 's3': {'bucket': 'panko-state', 'region': 'us-east-1', 'endpoint': 'http://moto:5000', 'usePathStyle': True}}},
        {'apiVersion': 'v1', 'kind': 'Secret', 'metadata': {'name': 'moto-state', 'namespace': TEAM}, 'stringData': {'access_key_id': 'test', 'secret_access_key': 'test'}},
        {'apiVersion': 'platform.example.com/v1', 'kind': 'StateBackend', 'metadata': {'name': 'moto', 'namespace': TEAM},
         'spec': {'profileRef': {'name': 'moto'}, 'credentialsSecretRef': {'name': 'moto-state'}}}]})
    kube('apply', '-f', 'test/bucket-controller.yaml')
    kube('apply', '-f', '-', obj=json.loads(admin('gateway-rbac')))
    return first, config_path


def isolated_from_cluster():
    """A cluster pod reaches the Kubernetes API (proving the probe works) but not Moto."""
    ip = docker('inspect', '-f', '{{(index .NetworkSettings.Networks "' + NETWORK + '").IPAddress}}', NAMES['moto']).stdout.strip()
    pod = 'cloud-probe-' + SUFFIX
    script = f'nc -z -w 4 kubernetes.default.svc 443 || exit 3; nc -z -w 4 {ip} 5000 && exit 4; exit 0'
    kube('create', '-f', '-', obj={'apiVersion': 'v1', 'kind': 'Pod', 'metadata': {'name': pod, 'namespace': 'team-a'},
         'spec': {'restartPolicy': 'Never', 'automountServiceAccountToken': False,
                  'containers': [{'name': 'probe', 'image': 'postgres:17.11-alpine', 'command': ['sh', '-c', script]}]}})
    try:
        wait(lambda: get('pod', pod)['status'].get('phase') in ('Succeeded', 'Failed'), 'probe', 120)
        terminated = get('pod', pod)['status']['containerStatuses'][0]['state']['terminated']
        assert terminated['exitCode'] != 3, 'the probe cannot reach the Kubernetes API, so it proves nothing'
        return terminated['exitCode'] == 0
    finally:
        kube('delete', 'pod', pod, '-n', 'team-a', '--wait=false', check=False)


def main():
    name = 'cloud-' + SUFFIX
    path = f'/v1/teams/{TEAM}/buckets'
    root = Path(tempfile.mkdtemp(prefix='remote-e2e-', dir='.local')).resolve()
    forward = None
    try:
        first, config_path = setup(root)
        forward = subprocess.Popen(K + ['-n', 'infra-gateway', 'port-forward', 'service/platform-gateway', '18085:8080'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        wait(lambda: subprocess.run(['curl', '--silent', '--fail', '--max-time', '5', URL + '/openapi.json'], capture_output=True).stdout.find(b'/v1/teams/{team}/buckets') >= 0, 'gateway knows Buckets', 60)

        curl('POST', path, {'name': name, 'spec': {'tags': {'env': 'test'}, 'stateBackendRef': {'name': 'moto'}}}, expected=201)
        created = ready(name)
        uid = get('bucket', name, TEAM)['metadata']['uid']
        assert created['runnerPool'] == 'cloud', created
        runs = json.loads(kube('get', 'runs', '-n', TEAM, '-l', 'platform.example.com/uid=' + uid, '-o', 'json').stdout)['items']
        jobs = json.loads(kube('get', 'jobs', '-n', TEAM, '-l', 'platform.example.com/uid=' + uid, '-o', 'json').stdout)['items']
        assert runs and not jobs, 'the bucket did not run on the remote pool'
        assert all(r['spec']['leaseSeconds'] == 25 and r['spec']['heartbeatSeconds'] == 3 for r in runs), 'pool lease not applied'
        assert 'secret_access_key' not in json.dumps(runs) and 'AWS_SECRET' not in json.dumps(runs), 'credentials in a Run'
        assert bucket(name) == {'env': 'test', 'managed-by': 'panko'}, bucket(name)
        print('1: bucket created in the emulated cloud by the remote agent; a Run instead of a Job, no credentials in it', flush=True)

        assert isolated_from_cluster(), 'cluster pods reach the emulated cloud'
        print('2: the emulated cloud is reachable for the agent only', flush=True)

        curl('PATCH', f'{path}/{name}', {'spec': {'tags': {'env': 'prod'}}})
        ready(name, created['lastAppliedHash'])
        assert bucket(name)['env'] == 'prod', bucket(name)
        print('3: tag change applied by the agent', flush=True)

        tamper(name, {'env': 'tampered', 'managed-by': 'panko'})
        curl('POST', f'{path}/{name}/plan', {}, expected=202)
        plan = wait(lambda: (status(name).get('lastPlan') or {}).get('drift') is not None and status(name)['lastPlan'], 'drift check', 300)
        assert plan['drift'] is True and plan['result'] == 'Succeeded', plan
        assert bucket(name)['env'] == 'tampered', 'the drift check repaired the bucket'
        print('4: manual change in the cloud detected as drift; nothing was changed', flush=True)

        graph = curl('GET', f'{path}/{name}/graph')
        assert any(node['label'] == 'aws_s3_bucket.this' for node in graph['nodes']), graph
        print('5: resource graph from the remote apply', flush=True)

        previous = status(name)['lastAppliedHash']
        write_profile(first, AWS_SECRET_ACCESS_KEY=None)
        curl('PATCH', f'{path}/{name}', {'spec': {'tags': {'env': 'retried'}}})
        failed = wait(lambda: (s := status(name)).get('phase') == 'Failed' and s, 'failed apply', 300)
        assert failed['lastExecution']['code'] == 'credentials', failed['lastExecution']
        write_profile(first)
        curl('POST', f'{path}/{name}/retry', {}, expected=202)
        ready(name, previous, after=failed['lastExecution']['attempt'])
        assert bucket(name)['env'] == 'retried', bucket(name)
        print('6: agent-side failure reported with its code; retry after fixing the profile succeeded', flush=True)

        # An agent dies mid-apply: its lease runs out, the Run is interrupted, the operator removes the
        # state lock it may have left, and a second agent finishes the explicit retry.
        previous = status(name)['lastAppliedHash']
        write_profile(first, TF_VAR_pause='120')
        curl('PATCH', f'{path}/{name}', {'spec': {'tags': {'env': 'recovered'}}})
        # Kill the agent while OpenTofu holds the state lock in the middle of this paused apply, not
        # while some other execution happens to hold it.
        def apply_holds_lock():
            items = json.loads(kube('get', 'runs', '-n', TEAM, '-l', 'platform.example.com/uid=' + uid, '-o', 'json').stdout)['items']
            generation = get('bucket', name, TEAM)['metadata']['generation']
            claimed = [r for r in items if r['spec']['action'] == 'apply' and r['spec']['generation'] == generation
                       and (r.get('status') or {}).get('lease') and not (r.get('status') or {}).get('conditions')]
            return claimed and state_locks()
        wait(apply_holds_lock, 'state lock held by the first agent for the new apply', 300)
        docker('kill', NAMES['agent'])
        interrupted = wait(lambda: (s := status(name)).get('phase') == 'Failed' and s, 'interrupted apply', 300)
        assert interrupted['lastExecution']['code'] == 'interrupted', interrupted['lastExecution']
        locks = remove_state_locks()
        assert locks, 'the killed agent left no state lock, so recovery was not exercised'
        assert not state_locks()
        second = agent_directory(root, 'second', config_path)
        start_agent(NAMES['second'], second)
        curl('POST', f'{path}/{name}/retry', {}, expected=202)
        ready(name, previous, after=interrupted['lastExecution']['attempt'])
        assert bucket(name)['env'] == 'recovered', bucket(name)
        print(f'7: killed agent reported as interrupted; after removing {len(locks)} leftover state lock(s) a second agent finished the retry', flush=True)

        curl('DELETE', f'{path}/{name}', expected=202)
        wait(lambda: get('bucket', name, TEAM) is None, 'bucket deleted', 600)
        assert bucket(name) is None, 'bucket still in the emulated cloud'
        print('8: deletion destroyed the bucket through the remote agent', flush=True)
        print('All remote runner end-to-end tests passed', flush=True)
    except BaseException:
        diagnostics(name)
        raise
    finally:
        cleanup(name, root, forward)


def diagnostics(name):
    """Best effort: each part is printed on its own, so one failing lookup hides nothing else."""
    def show(title, produce):
        try:
            print('--- ' + title, produce(), sep='\n', flush=True)
        except Exception as e:
            print('--- ' + title, 'unavailable:', type(e).__name__, flush=True)
    for container in (NAMES['agent'], NAMES['second']):
        show(container, lambda container=container: (lambda r: r.stdout + r.stderr)(docker('logs', '--tail', '60', container, check=False, timeout=30)))
    show('bucket status', lambda: json.dumps(status(name), indent=1))
    show('runs', lambda: kube('get', 'runs', '-n', TEAM, '-o', 'wide', check=False).stdout)
    show('endpoint', lambda: kube('-n', 'infra-runners', 'logs', 'deploy/runner-endpoint', '--tail=40', check=False).stdout)


def cleanup(name, root, forward):
    """Every step runs even if an earlier one fails, so a failed test leaves no pool, agent or token."""
    def step(action):
        try:
            action()
        except Exception:
            traceback.print_exc()

    def delete_bucket():
        if kube('get', 'crd', 'buckets.platform.example.com', check=False).returncode != 0:
            return
        kube('delete', 'bucket', name, '-n', TEAM, '--ignore-not-found', '--wait=false', check=False)
        try:
            wait(lambda: get('bucket', name, TEAM) is None, 'cleanup', 300)
        except AssertionError:
            # A test resource that cannot be destroyed must not block the lab; the bucket itself
            # lived only in the emulator.
            print('cleanup: removing the finalizer of a bucket that could not be destroyed', flush=True)
            kube('patch', 'bucket', name, '-n', TEAM, '--type=merge', '-p', '{"metadata":{"finalizers":null}}', check=False)

    def restore_pools():
        kube('apply', '-f', '-', obj=json.loads(admin('runner-pools')))
        kube('-n', 'infra-runners', 'rollout', 'restart', 'deploy/database-hook')
        kube('-n', 'infra-runners', 'rollout', 'status', 'deploy/database-hook', '--timeout=120s')

    def revoke_agents():
        for label in ('agent', 'second'):
            token = root / label / 'token'
            if token.exists():
                admin('revoke-agent', '--agent-id', token.read_text().split('.')[0])

    step(delete_bucket)
    if forward:
        forward.terminate()
    step(restore_pools)
    step(lambda: kube('delete', '-f', 'test/bucket-controller.yaml', '--ignore-not-found'))
    step(lambda: kube('delete', '-f', 'test/bucket-crd.yaml', '--ignore-not-found'))
    step(lambda: kube('delete', 'statebackend,backendprofile', 'moto', '-n', TEAM, '--ignore-not-found'))
    step(lambda: kube('delete', 'secret', 'moto-state', '-n', TEAM, '--ignore-not-found'))
    step(lambda: kube('apply', '-f', '-', obj=json.loads(admin('gateway-rbac'))))
    step(revoke_agents)
    for container in NAMES.values():
        docker('rm', '-f', container, check=False)
    docker('network', 'rm', NETWORK, check=False)
    shutil.rmtree(root, ignore_errors=True)


if __name__ == '__main__':
    main()
