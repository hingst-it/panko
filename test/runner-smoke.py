"""Exercise the runner as an external Job, independent of its implementation language."""
import copy
import hashlib
import json
import os
from pathlib import Path
import time

from e2e import credentials, get, kube, sql, wait


def main():
    name = 'runner-smoke-' + str(time.time_ns())
    job_names = []
    replicas = get('deployment', 'database-hook', 'infra-runners')['spec']['replicas']
    creds = None
    try:
        kube('scale', 'deploy/database-hook', '-n', 'infra-runners', '--replicas=0')
        wait(lambda: not json.loads(kube('get', 'pods', '-n', 'infra-runners', '-l',
                                        'app=database-hook', '-o', 'json').stdout)['items'], 'hook stopped')
        kube('create', '-f', '-', obj={'apiVersion': 'platform.example.com/v1', 'kind': 'Database',
             'metadata': {'name': name, 'namespace': 'team-a'},
             'spec': {'owner': 'team-a', 'extensions': ['pgcrypto'], 'connectionLimit': 20}})
        parent = get('databases.platform.example.com', name)
        snapshot = {'metadata': {k: parent['metadata'][k] for k in ('name', 'namespace', 'uid', 'generation')},
                    'spec': parent['spec']}
        digest = hashlib.sha256(json.dumps({'spec': snapshot['spec'], 'generation': snapshot['metadata']['generation']},
                                          sort_keys=True, separators=(',', ':')).encode()).hexdigest()
        fixture = json.loads(Path('testdata/reconcile.json').read_text())[0]['expected']['children'][0]
        # The fixture holds test values; run the engine and module the deployed hook is configured with.
        hook_env = {item['name']: item.get('value', '') for item in
                    get('deployment', 'database-hook', 'infra-runners')['spec']['template']['spec']['containers'][0].get('env', [])}
        text = json.dumps(fixture)
        source = hook_env.get('MODULE_MIRROR') or hook_env['MODULE_REPO']
        for test_value, value in (('engine:test', hook_env['ENGINE_IMAGE']), ('git-sync:test', hook_env['GIT_SYNC_IMAGE']),
                                  ('git@example.com:panko/module.git', source), ('1' * 40, hook_env['MODULE_REVISION'])):
            text = text.replace(test_value, value)
        fixture = json.loads(text)
        if hook_env.get('MODULE_MIRROR'):
            # Like the hook: the mirror needs no key, which no longer exists in team namespaces.
            pod = fixture['spec']['template']['spec']
            pod['volumes'] = [v for v in pod['volumes'] if v['name'] != 'git-secret']
            for c in pod['initContainers']:
                c['volumeMounts'] = [m for m in c['volumeMounts'] if m['name'] != 'git-secret']
        for action in ('apply', 'destroy'):
            job_name = name + '-' + action
            child = copy.deepcopy(fixture)
            child['metadata'] = {'name': job_name, 'namespace': 'team-a'}
            container = child['spec']['template']['spec']['containers'][0]
            # The shared panko-server image runs the runner role, as the hook's Jobs do.
            container['image'] = os.environ.get('RUNNER_IMAGE', hook_env.get('RUNNER_IMAGE', 'panko-server:dev'))
            container['imagePullPolicy'] = hook_env.get('RUNNER_IMAGE_PULL_POLICY', 'Never')
            container['args'] = hook_env.get('RUNNER_COMMAND', '').split() + [action, 'team-a', name]
            backend = {'type': 'kubernetes', 'config': {'in_cluster_config': True, 'namespace': 'panko-state-team-a',
                                                        'secret_suffix': 'team-a-' + name}}
            values = {'PANKO_RESOURCE': json.dumps(snapshot), 'PANKO_HASH': digest, 'PANKO_JOB': job_name,
                      'PANKO_BACKEND': json.dumps(backend)}
            container['env'] = [item for item in container['env'] if item['name'] not in values] + [
                {'name': key, 'value': value} for key, value in values.items()]
            kube('create', '-f', '-', obj=child)
            job_names.append(job_name)
            def completed():
                job = get('job', job_name)
                for condition in job.get('status', {}).get('conditions', []):
                    if condition['status'] == 'True' and condition['type'] == 'Failed':
                        raise AssertionError('Runner Job failed: ' + job_name)
                    if condition['status'] == 'True' and condition['type'] == 'Complete':
                        return True
                return False
            wait(completed, 'runner ' + action)
            if action == 'apply':
                creds = credentials(name)
                assert sql(f"SELECT count(*) FROM pg_database WHERE datname='{creds['database']}';") == '1'
        assert get('secret', 'tfstate-default-team-a-' + name, 'panko-state-team-a') is None
        assert get('lease', 'lock-tfstate-default-team-a-' + name, 'panko-state-team-a') is None
        assert get('secret', name + '-connection') is None
        assert sql(f"SELECT count(*) FROM pg_database WHERE datname='{creds['database']}';") == '0'
        assert sql(f"SELECT count(*) FROM pg_roles WHERE rolname='{creds['username']}';") == '0'
        print('External runner smoke: apply, owned connection Secret, destroy, state and lock cleanup passed', flush=True)
    finally:
        kube('delete', 'databases.platform.example.com', name, '-n', 'team-a', '--ignore-not-found', '--wait=false', check=False)
        for job_name in job_names:
            kube('delete', 'job', job_name, '-n', 'team-a', '--ignore-not-found', '--wait=false', check=False)
        kube('scale', 'deploy/database-hook', '-n', 'infra-runners', '--replicas=' + str(replicas))
        kube('rollout', 'status', 'deploy/database-hook', '-n', 'infra-runners', '--timeout=120s')
        if replicas:
            wait(lambda: get('databases.platform.example.com', name) is None, 'smoke resource finalized')


if __name__ == '__main__':
    main()
