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
        parent = get('db', name)
        snapshot = {'metadata': {k: parent['metadata'][k] for k in ('name', 'namespace', 'uid', 'generation')},
                    'spec': parent['spec']}
        digest = hashlib.sha256(json.dumps({'spec': snapshot['spec'], 'generation': snapshot['metadata']['generation']},
                                          sort_keys=True, separators=(',', ':')).encode()).hexdigest()
        fixture = json.loads(Path('testdata/reconcile.json').read_text())[0]['expected']['children'][0]
        # The fixture holds test values; run the engine and module the deployed hook is configured with.
        hook_env = {item['name']: item.get('value', '') for item in
                    get('deployment', 'database-hook', 'infra-runners')['spec']['template']['spec']['containers'][0].get('env', [])}
        text = json.dumps(fixture)
        for test_value, key in (('engine:test', 'ENGINE_IMAGE'), ('git-sync:test', 'GIT_SYNC_IMAGE'),
                                ('git@example.com:panko/module.git', 'MODULE_REPO'), ('1' * 40, 'MODULE_REVISION')):
            text = text.replace(test_value, hook_env[key])
        fixture = json.loads(text)
        for action in ('apply', 'destroy'):
            job_name = name + '-' + action
            child = copy.deepcopy(fixture)
            child['metadata'] = {'name': job_name, 'namespace': 'team-a'}
            container = child['spec']['template']['spec']['containers'][0]
            container['image'] = os.environ.get('RUNNER_IMAGE', 'panko-runner:dev')
            container['args'] = [action, 'team-a', name]
            values = {'PANKO_RESOURCE': json.dumps(snapshot), 'PANKO_HASH': digest, 'PANKO_JOB': job_name}
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
        kube('delete', 'db', name, '-n', 'team-a', '--ignore-not-found', '--wait=false', check=False)
        for job_name in job_names:
            kube('delete', 'job', job_name, '-n', 'team-a', '--ignore-not-found', '--wait=false', check=False)
        kube('scale', 'deploy/database-hook', '-n', 'infra-runners', '--replicas=' + str(replicas))
        kube('rollout', 'status', 'deploy/database-hook', '-n', 'infra-runners', '--timeout=120s')
        if replicas:
            wait(lambda: get('db', name) is None, 'smoke resource finalized')


if __name__ == '__main__':
    main()
