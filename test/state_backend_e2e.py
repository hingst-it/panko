"""Team-defined state backends: binding, isolation, safety checks and administrator migration."""
import json
import os
import subprocess
import time

from e2e import RUN, credentials, delete_and_check, get, kube, ready, sql, wait

AWS_CLI = 'amazon/aws-cli:2.38.5@sha256:c8156a0e74e13929a42d7e60f0d4e14e7d28d980cd21822505d4bff80e5460c1'
ENDPOINT = 'http://s3.infra-backends.svc.cluster.local:7070'
CONFIG = os.environ.get('KUBECONFIG', '.local/kubeconfig')


def database(name, ref=None, limit=20):
    spec = {'owner': 'team-a', 'extensions': [], 'connectionLimit': limit}
    if ref:
        spec['stateBackendRef'] = {'name': ref}
    kube('create', '-f', '-', obj={'apiVersion': 'platform.example.com/v1', 'kind': 'Database',
         'metadata': {'name': name, 'namespace': 'team-a', 'labels': {'test-run': RUN}}, 'spec': spec})


def state_backend(name):
    kube('apply', '-f', '-', obj={'apiVersion': 'platform.example.com/v1', 'kind': 'StateBackend',
         'metadata': {'name': name, 'namespace': 'team-a'},
         'spec': {'profileRef': {'name': 'lab-s3'}, 'credentialsSecretRef': {'name': 'panko-state-s3'}}})


def binding(name):
    return get('db', name)['status']['stateBackend']


def s3(team, *args):
    """Run the AWS CLI with a team's own key; returns (exit code, output)."""
    pod = 's3-check-' + str(time.time_ns())
    env = [{'name': 'AWS_ACCESS_KEY_ID', 'valueFrom': {'secretKeyRef': {'name': 'panko-state-s3', 'key': 'access_key_id'}}},
           {'name': 'AWS_SECRET_ACCESS_KEY', 'valueFrom': {'secretKeyRef': {'name': 'panko-state-s3', 'key': 'secret_access_key'}}},
           {'name': 'AWS_DEFAULT_REGION', 'value': 'us-east-1'}]
    kube('create', '-f', '-', obj={'apiVersion': 'v1', 'kind': 'Pod', 'metadata': {'name': pod, 'namespace': team},
         'spec': {'restartPolicy': 'Never', 'automountServiceAccountToken': False,
                  'containers': [{'name': 'aws', 'image': AWS_CLI, 'args': ['--endpoint-url', ENDPOINT, *args], 'env': env}]}})
    try:
        phase = wait(lambda: get('pod', pod, team)['status'].get('phase') in ('Succeeded', 'Failed') and get('pod', pod, team)['status']['phase'],
                     'S3 check pod', 120)
        return (0 if phase == 'Succeeded' else 1), kube('logs', pod, '-n', team, check=False).stdout
    finally:
        kube('delete', 'pod', pod, '-n', team, '--wait=false', check=False)


def failed_with(name, generation, text):
    def check():
        status = get('db', name).get('status', {})
        return status.get('phase') == 'Failed' and status.get('observedGeneration') == generation
    wait(check, f'{name} Failed at generation {generation}')
    assert get('db', name)['status']['lastExecution']['code'] == 'checkpoint', get('db', name)['status']
    jobs = json.loads(kube('get', 'jobs', '-n', 'team-a', '-l', 'platform.example.com/uid=' + get('db', name)['metadata']['uid'],
                           '-o', 'json').stdout)['items']
    logs = ''.join(kube('logs', 'job/' + j['metadata']['name'], '-n', 'team-a', '-c', 'tofu', check=False).stdout for j in jobs)
    assert text in logs, logs[-2000:]


def main():
    fallback, remote, by_default, missing = (f'sb-{kind}-{RUN[-6:]}' for kind in ('fb', 's3', 'def', 'miss'))
    names = [fallback, remote, by_default, missing]
    target = 'lab-' + RUN[-6:]
    try:
        # 1: without any StateBackend the built-in Kubernetes backend is bound, now with a checkpoint.
        database(fallback)
        ready(fallback)
        assert binding(fallback)['type'] == 'kubernetes'
        uid = get('db', fallback)['metadata']['uid']
        assert get('secret', f'tfstate-default-team-a-{fallback}', 'panko-state-team-a') is not None
        assert get('secret', 'panko-checkpoint-' + uid, 'panko-state-team-a') is not None
        print('1: fallback binding, Kubernetes state and checkpoint created', flush=True)

        # 2: an explicit StateBackend binds the operator-approved bucket; the key carries installation and UID.
        state_backend(target)
        database(remote, ref=target)
        ready(remote)
        bound = binding(remote)
        remote_uid = get('db', remote)['metadata']['uid']
        key = bound['config']['key']
        assert bound['type'] == 's3' and key.endswith(f'/{remote_uid}/terraform.tfstate'), bound
        assert get('secret', f'tfstate-default-team-a-{remote}', 'panko-state-team-a') is None
        code, _ = s3('team-a', 's3api', 'head-object', '--bucket', 'panko-state-team-a', '--key', key)
        assert code == 0, 'state object missing in the team bucket'
        creds = credentials(remote)
        assert sql(f"SELECT count(*) FROM pg_database WHERE datname='{creds['database']}';") == '1'
        print('2: explicit StateBackend bound to the team bucket; database provisioned with S3 state', flush=True)

        # 3: another team's key cannot read this team's state.
        code, out = s3('team-b', 's3api', 'head-object', '--bucket', 'panko-state-team-a', '--key', key)
        assert code != 0 and ('403' in out or 'Forbidden' in out or 'AccessDenied' in out), out
        print('3: team-b key denied access to team-a state', flush=True)

        # 4: a later namespace default never moves an existing resource, but binds new ones.
        state_backend('default')
        kube('patch', 'db', fallback, '-n', 'team-a', '--type=merge', '-p', '{"spec":{"connectionLimit":21}}')
        ready(fallback, generation=2)
        assert binding(fallback)['type'] == 'kubernetes'
        database(by_default)
        ready(by_default)
        assert binding(by_default)['type'] == 's3' and binding(by_default)['stateBackend'] == 'default'
        print('4: default StateBackend binds new resources only; existing binding unchanged', flush=True)

        # 5: the reference is immutable once created.
        result = kube('patch', 'db', remote, '-n', 'team-a', '--type=merge', '-p', '{"spec":{"stateBackendRef":{"name":"default"}}}', check=False)
        assert result.returncode and 'immutable' in result.stderr, result.stderr
        print('5: stateBackendRef change rejected by the API server', flush=True)

        # 6: a missing explicit backend fails without falling back, and without a job.
        database(missing, ref='does-not-exist')
        wait(lambda: get('db', missing).get('status', {}).get('phase') == 'Failed', 'missing backend reported')
        status = get('db', missing)['status']
        assert 'stateBackend' not in status and 'not found' in status['stateBackendError'], status
        assert not json.loads(kube('get', 'jobs', '-n', 'team-a', '-l', 'platform.example.com/uid=' + get('db', missing)['metadata']['uid'],
                                   '-o', 'json').stdout)['items']
        kube('delete', 'db', missing, '-n', 'team-a', '--wait=false')
        wait(lambda: get('db', missing) is None, 'never-provisioned resource finalized', 60)
        print('6: missing StateBackend fails without fallback; deletion needs no teardown', flush=True)

        # 7: lost state is detected and blocks the next apply until the state is restored.
        state_name = f'tfstate-default-team-a-{fallback}'
        saved = get('secret', state_name, 'panko-state-team-a')
        kube('delete', 'secret', state_name, '-n', 'panko-state-team-a')
        kube('patch', 'db', fallback, '-n', 'team-a', '--type=merge', '-p', '{"spec":{"connectionLimit":22}}')
        # The Kubernetes backend recreates an empty state with a new lineage on init; either way the apply is blocked.
        failed_with(fallback, 3, 'state safety check failed')
        restored = {k: saved[k] for k in ('apiVersion', 'kind', 'type', 'data')}
        restored['metadata'] = {k: saved['metadata'][k] for k in ('name', 'namespace', 'labels', 'annotations') if k in saved['metadata']}
        kube('delete', 'secret', state_name, '-n', 'panko-state-team-a', '--ignore-not-found')
        kube('create', '-f', '-', obj=restored)
        kube('patch', 'db', fallback, '-n', 'team-a', '--type=merge', '-p', '{"spec":{"connectionLimit":23}}')
        ready(fallback, generation=4)
        print('7: missing state blocked the apply; restored state applied normally', flush=True)

        # 8: administrator migration moves the Kubernetes state to S3; later applies and teardown use S3.
        migration = 'm-' + RUN[-6:]
        subprocess.run(['.local/bin/admin', '--kubeconfig', CONFIG, 'migrate-backend', '--namespace', 'team-a', '--name', fallback,
                        '--state-backend', target, '--migration-id', migration], check=True, capture_output=True, text=True)
        moved = binding(fallback)
        assert moved['type'] == 's3' and moved['stateBackend'] == target, moved
        assert 'platform.example.com/state-maintenance' not in get('db', fallback)['metadata'].get('annotations', {})
        assert get('secret', f'panko-backup-{uid}-{migration}', 'panko-state-team-a') is not None
        before = credentials(fallback)
        kube('patch', 'db', fallback, '-n', 'team-a', '--type=merge', '-p', '{"spec":{"connectionLimit":24}}')
        ready(fallback, generation=5)
        assert credentials(fallback) == before, 'migration recreated the database instead of keeping it'
        rerun = subprocess.run(['.local/bin/admin', '--kubeconfig', CONFIG, 'migrate-backend', '--namespace', 'team-a', '--name', fallback,
                                '--state-backend', target, '--migration-id', migration], capture_output=True, text=True)
        assert rerun.returncode == 0 and 'already completed' in rerun.stdout, rerun.stdout + rerun.stderr
        # The source copy is retained for recovery; removing it is an explicit operator step.
        assert get('secret', f'tfstate-default-team-a-{fallback}', 'panko-state-team-a') is not None
        kube('delete', 'secret', f'tfstate-default-team-a-{fallback}', '-n', 'panko-state-team-a')
        print('8: migration copied and switched the state; next apply kept the database; rerun is a no-op', flush=True)

        for name in (fallback, remote, by_default):
            delete_and_check(name, 'team-a', credentials(name) if name != fallback else before)
        print('All state backend end-to-end tests passed', flush=True)
    finally:
        for name in names:
            kube('delete', 'db', name, '-n', 'team-a', '--ignore-not-found', '--wait=false', check=False)
        for name in names:
            wait(lambda name=name: get('db', name) is None, f'{name} cleanup', 300)
        kube('delete', 'statebackend', target, 'default', '-n', 'team-a', '--ignore-not-found', check=False)
        # The migrated source state and its backup are retained by design; remove the test copies.
        kube('delete', 'secret', '-n', 'panko-state-team-a', f'tfstate-default-team-a-{fallback}', '--ignore-not-found', check=False)
        for secret in json.loads(kube('get', 'secrets', '-n', 'panko-state-team-a', '-o', 'json').stdout)['items']:
            if secret['metadata']['name'].startswith(('panko-backup-', 'panko-migration-')):
                kube('delete', 'secret', secret['metadata']['name'], '-n', 'panko-state-team-a', check=False)


if __name__ == '__main__':
    main()
