"""Remote runner pool against an emulated cloud (roadmap 8c): an agent outside the cluster provisions
an S3 bucket in Moto, which only the agent can reach, through the gateway and the runner endpoint."""
import json
import os
import shutil
import subprocess
import tempfile
from pathlib import Path

from e2e import K, RUN, get, kube, wait

CLUSTER = os.environ.get('CLUSTER_NAME', 'infra-lab')
CONFIG = os.environ.get('KUBECONFIG', '.local/kubeconfig')
MOTO = 'motoserver/moto:5.2.3'
TEAM = 'team-b'
SUFFIX = RUN[-6:]
NETWORK = 'panko-cloud-' + SUFFIX
NAMES = {'moto': 'moto-' + SUFFIX, 'git': 'git-' + SUFFIX, 'agent': 'agent-' + SUFFIX}
URL = 'http://127.0.0.1:18085'
TOKENS = json.loads(Path('.local/team-tokens.json').read_text())


def docker(*args, check=True):
    result = subprocess.run(['docker', *args], capture_output=True, text=True)
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
    out = moto(f"""
found = [b['Name'] for b in s3.list_buckets()['Buckets'] if b['Name'].startswith('panko-{name}-')]
print(json.dumps({{t['Key']: t['Value'] for t in s3.get_bucket_tagging(Bucket=found[0])['TagSet']}} if found else None))
""")
    return json.loads(out)


def tamper(name, tags):
    moto(f"""
found = [b['Name'] for b in s3.list_buckets()['Buckets'] if b['Name'].startswith('panko-{name}-')]
s3.put_bucket_tagging(Bucket=found[0], Tagging={{'TagSet': [{{'Key': k, 'Value': v}} for k, v in {tags!r}.items()]}})
""")


def ready(name, previous=None):
    def check():
        b = get('bucket', name, TEAM)
        status = b and b.get('status') or {}
        assert status.get('phase') != 'Failed', status
        if status.get('phase') == 'Ready' and status.get('lastAppliedHash') != previous:
            return b
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
    for path in [root, *root.rglob('*')]:
        path.chmod(0o755 if path.is_dir() else 0o644)
    return revision


def setup(root):
    docker('network', 'create', NETWORK)
    docker('run', '-d', '--name', NAMES['moto'], '--network', NETWORK, '--network-alias', 'moto', '-e', 'S3_IGNORE_SUBDOMAIN_BUCKETNAME=true', MOTO)
    wait(lambda: docker('exec', NAMES['moto'], 'python', '-c', 'import urllib.request; urllib.request.urlopen("http://localhost:5000")', check=False).returncode == 0, 'Moto', 60)
    moto("s3.create_bucket(Bucket='panko-state')")
    docker('build', '-q', '-t', 'panko-agent:dev', '-f', 'Dockerfile.agent', '.')
    revision = fixture_repository(root)
    docker('run', '-d', '--name', NAMES['git'], '--network', NETWORK, '--network-alias', 'git', '-v', f'{root / "repos"}:/repos:ro',
           '--entrypoint', 'git', 'panko-agent:dev', '-c', 'safe.directory=*', 'daemon', '--base-path=/repos', '--export-all', '--reuseaddr', '/repos')
    # The pool, its module and its agent, configured exactly as an operator would.
    config = json.loads(Path(os.environ.get('PANKO_CONFIG', 'panko.json')).read_text())
    config['runnerPools'] = [{'name': 'cloud', 'teams': [TEAM], 'module': {'repository': 'git://git/modules.git', 'revision': revision, 'path': 'aws-s3'}}]
    config_path = root / 'panko.json'
    config_path.write_text(json.dumps(config))
    kube('apply', '-f', '-', obj=json.loads(admin('runner-pools', config=str(config_path))))
    kube('-n', 'infra-runners', 'rollout', 'restart', 'deploy/database-hook')
    kube('-n', 'infra-runners', 'rollout', 'status', 'deploy/database-hook', '--timeout=120s')
    agent = root / 'agent'
    (agent / 'profiles').mkdir(parents=True)
    agent.chmod(0o700)
    (agent / 'profiles').chmod(0o700)
    (agent / 'token').write_text(admin('add-agent', '--pool', 'cloud', config=str(config_path)).strip())
    (agent / 'token').chmod(0o600)
    (agent / 'ca.crt').write_text(admin('endpoint-ca'))
    write_profile(agent)
    node = 'k3d-' + CLUSTER + '-server-0'
    ip = docker('inspect', '-f', '{{(index .NetworkSettings.Networks "k3d-' + CLUSTER + '").IPAddress}}', node).stdout.strip()
    docker('run', '-d', '--name', NAMES['agent'], '--network', 'k3d-' + CLUSTER, '--add-host', f'runner-endpoint.infra-runners.svc:{ip}',
           '--user', f'{os.getuid()}:{os.getgid()}', '-v', f'{agent}:/etc/panko-agent:ro',
           '-e', 'PANKO_ENDPOINT=https://runner-endpoint.infra-runners.svc:30443', '-e', 'PANKO_ENDPOINT_CA=/etc/panko-agent/ca.crt',
           '-e', 'PANKO_AGENT_TOKEN_FILE=/etc/panko-agent/token', '-e', 'PANKO_POOL=cloud', '-e', 'PANKO_PROFILES=/etc/panko-agent/profiles',
           '-e', 'PANKO_WORK=/tmp', '-e', 'PANKO_PLUGIN_CACHE=/tmp/plugins', 'panko-agent:dev', 'run')
    docker('network', 'connect', '--alias', 'agent', NETWORK, NAMES['agent'])
    check = docker('exec', NAMES['agent'], 'panko-agent', 'check', check=False)
    assert check.returncode == 0, check.stdout + check.stderr
    # The team's S3 state backend lives in Moto as well; its credentials are in the agent's profile.
    kube('apply', '-f', '-', obj={'apiVersion': 'v1', 'kind': 'List', 'items': [
        {'apiVersion': 'platform.example.com/v1', 'kind': 'BackendProfile', 'metadata': {'name': 'moto', 'namespace': TEAM},
         'spec': {'type': 's3', 's3': {'bucket': 'panko-state', 'region': 'us-east-1', 'endpoint': 'http://moto:5000', 'usePathStyle': True}}},
        {'apiVersion': 'v1', 'kind': 'Secret', 'metadata': {'name': 'moto-state', 'namespace': TEAM}, 'stringData': {'access_key_id': 'test', 'secret_access_key': 'test'}},
        {'apiVersion': 'platform.example.com/v1', 'kind': 'StateBackend', 'metadata': {'name': 'moto', 'namespace': TEAM},
         'spec': {'profileRef': {'name': 'moto'}, 'credentialsSecretRef': {'name': 'moto-state'}}}]})
    kube('apply', '-f', 'test/bucket-crd.yaml')
    kube('wait', '--for=condition=Established', 'crd/buckets.platform.example.com', '--timeout=60s')
    kube('apply', '-f', 'test/bucket-controller.yaml')
    kube('apply', '-f', '-', obj=json.loads(admin('gateway-rbac')))
    return agent


def unreachable_from_cluster():
    ip = docker('inspect', '-f', '{{(index .NetworkSettings.Networks "' + NETWORK + '").IPAddress}}', NAMES['moto']).stdout.strip()
    pod = 'cloud-probe-' + SUFFIX
    kube('create', '-f', '-', obj={'apiVersion': 'v1', 'kind': 'Pod', 'metadata': {'name': pod, 'namespace': 'team-a'},
         'spec': {'restartPolicy': 'Never', 'automountServiceAccountToken': False,
                  'containers': [{'name': 'probe', 'image': 'postgres:17.11-alpine', 'command': ['nc', '-z', '-w', '4', ip, '5000']}]}})
    try:
        return wait(lambda: get('pod', pod)['status'].get('phase') in ('Succeeded', 'Failed') and get('pod', pod)['status']['phase'], 'probe', 120) == 'Failed'
    finally:
        kube('delete', 'pod', pod, '-n', 'team-a', '--wait=false', check=False)


def main():
    name = 'cloud-' + SUFFIX
    path = f'/v1/teams/{TEAM}/buckets'
    root = Path(tempfile.mkdtemp(prefix='remote-e2e-', dir='.local')).resolve()
    forward = None
    try:
        agent = setup(root)
        forward = subprocess.Popen(K + ['-n', 'infra-gateway', 'port-forward', 'service/platform-gateway', '18085:8080'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        wait(lambda: subprocess.run(['curl', '--silent', '--fail', URL + '/openapi.json'], capture_output=True).stdout.find(b'/v1/teams/{team}/buckets') >= 0, 'gateway knows Buckets', 60)

        curl('POST', path, {'name': name, 'spec': {'tags': {'env': 'test'}, 'stateBackendRef': {'name': 'moto'}}}, expected=201)
        created = ready(name)
        uid = created['metadata']['uid']
        assert created['status']['runnerPool'] == 'cloud', created['status']
        runs = json.loads(kube('get', 'runs', '-n', TEAM, '-l', 'platform.example.com/uid=' + uid, '-o', 'json').stdout)['items']
        jobs = json.loads(kube('get', 'jobs', '-n', TEAM, '-l', 'platform.example.com/uid=' + uid, '-o', 'json').stdout)['items']
        assert runs and not jobs, 'the bucket did not run on the remote pool'
        assert 'secret_access_key' not in json.dumps(runs) and 'AWS_SECRET' not in json.dumps(runs), 'credentials in a Run'
        assert bucket(name) == {'env': 'test', 'managed-by': 'panko'}, bucket(name)
        print('1: bucket created in the emulated cloud by the remote agent; a Run instead of a Job, no credentials in it', flush=True)

        assert unreachable_from_cluster(), 'cluster pods reach the emulated cloud'
        print('2: the emulated cloud is reachable for the agent only', flush=True)

        curl('PATCH', f'{path}/{name}', {'spec': {'tags': {'env': 'prod'}}})
        ready(name, created['status']['lastAppliedHash'])
        assert bucket(name)['env'] == 'prod', bucket(name)
        print('3: tag change applied by the agent', flush=True)

        tamper(name, {'env': 'tampered', 'managed-by': 'panko'})
        curl('POST', f'{path}/{name}/plan', {}, expected=202)
        plan = wait(lambda: (get('bucket', name, TEAM)['status'].get('lastPlan') or {}).get('drift') is not None and get('bucket', name, TEAM)['status']['lastPlan'], 'drift check', 300)
        assert plan['drift'] is True and plan['result'] == 'Succeeded', plan
        assert bucket(name)['env'] == 'tampered', 'the drift check repaired the bucket'
        print('4: manual change in the cloud detected as drift; nothing was changed', flush=True)

        graph = curl('GET', f'{path}/{name}/graph')
        assert any(node['label'] == 'aws_s3_bucket.this' for node in graph['nodes']), graph
        print('5: resource graph from the remote apply', flush=True)

        previous = get('bucket', name, TEAM)['status']['lastAppliedHash']
        write_profile(agent, AWS_SECRET_ACCESS_KEY=None)
        curl('PATCH', f'{path}/{name}', {'spec': {'tags': {'env': 'retried'}}})
        failed = wait(lambda: (s := get('bucket', name, TEAM)['status']).get('phase') == 'Failed' and s, 'failed apply', 300)
        assert failed['lastExecution']['code'] == 'credentials', failed['lastExecution']
        write_profile(agent)
        curl('POST', f'{path}/{name}/retry', {}, expected=202)
        ready(name, previous)
        assert bucket(name)['env'] == 'retried', bucket(name)
        print('6: agent-side failure reported with its code; retry after fixing the profile succeeded', flush=True)

        curl('DELETE', f'{path}/{name}', expected=202)
        wait(lambda: get('bucket', name, TEAM) is None, 'bucket deleted', 600)
        assert bucket(name) is None, 'bucket still in the emulated cloud'
        print('7: deletion destroyed the bucket through the remote agent', flush=True)
        print('All remote runner end-to-end tests passed', flush=True)
    except BaseException:
        print(docker('logs', '--tail', '80', NAMES['agent'], check=False).stdout, flush=True)
        raise
    finally:
        kube('delete', 'bucket', name, '-n', TEAM, '--ignore-not-found', '--wait=false', check=False)
        wait(lambda: get('bucket', name, TEAM) is None or kube('get', 'crd', 'buckets.platform.example.com', check=False).returncode != 0, 'cleanup', 300)
        if forward:
            forward.terminate()
        kube('apply', '-f', '-', obj=json.loads(admin('runner-pools')))
        kube('-n', 'infra-runners', 'rollout', 'restart', 'deploy/database-hook', check=False)
        kube('-n', 'infra-runners', 'rollout', 'status', 'deploy/database-hook', '--timeout=120s', check=False)
        kube('delete', '-f', 'test/bucket-controller.yaml', '--ignore-not-found', check=False)
        kube('delete', '-f', 'test/bucket-crd.yaml', '--ignore-not-found', check=False)
        kube('delete', 'statebackend,backendprofile', 'moto', '-n', TEAM, '--ignore-not-found', check=False)
        kube('delete', 'secret', 'moto-state', '-n', TEAM, '--ignore-not-found', check=False)
        rbac = subprocess.run(['.local/bin/admin', '--kubeconfig', CONFIG, 'gateway-rbac'], capture_output=True, text=True, check=False)
        if rbac.returncode == 0:
            kube('apply', '-f', '-', obj=json.loads(rbac.stdout), check=False)
        token = root / 'agent' / 'token'
        if token.exists():
            subprocess.run(['.local/bin/admin', '--kubeconfig', CONFIG, 'revoke-agent', '--agent-id', token.read_text().split('.')[0]], capture_output=True, check=False)
        for container in NAMES.values():
            docker('rm', '-f', container, check=False)
        docker('network', 'rm', NETWORK, check=False)
        shutil.rmtree(root, ignore_errors=True)


if __name__ == '__main__':
    main()
