"""Drift checks and manual module rollouts against a real lab."""
import json
import os
import subprocess
import time

from e2e import RUN, credentials, get, kube, ready, sql, wait

CONFIG = os.environ.get('KUBECONFIG', '.local/kubeconfig')
OLDER = 'a002bdcd497130e38d2cc2e526ebcb2187791232'  # an earlier module commit on the main branch


def database(name):
    kube('create', '-f', '-', obj={'apiVersion': 'platform.example.com/v1', 'kind': 'Database',
         'metadata': {'name': name, 'namespace': 'team-a', 'labels': {'test-run': RUN}},
         'spec': {'owner': 'team-a', 'extensions': [], 'connectionLimit': 20}})


def check(name, token):
    kube('annotate', 'databases.platform.example.com', name, '-n', 'team-a', f'platform.example.com/plan-request={token}', '--overwrite')
    # Evaluate one snapshot: Metacontroller can briefly write a status computed from an older one.
    def finished():
        plan = get('databases.platform.example.com', name)['status'].get('lastPlan', {})
        return plan if plan.get('token') == token else None
    plan = wait(finished, f'{name} drift check {token}', 300)
    assert plan['result'] == 'Succeeded', plan
    assert get('databases.platform.example.com', name)['status']['phase'] == 'Ready', 'drift check changed the phase'
    return plan


def jobs(name):
    uid = get('databases.platform.example.com', name)['metadata']['uid']
    return {j['metadata']['name'] for j in json.loads(kube('get', 'jobs', '-n', 'team-a', '-l', 'platform.example.com/uid=' + uid, '-o', 'json').stdout)['items']
            if j['metadata']['annotations'].get('platform.example.com/action') == 'apply'}


def hook_env(**values):
    kube('set', 'env', '-n', 'infra-runners', 'deploy/database-hook', *[f'{k}={v}' for k, v in values.items()])
    kube('rollout', 'status', '-n', 'infra-runners', 'deploy/database-hook', '--timeout=120s')


def main():
    name, fresh = 'drift-' + RUN[-6:], 'drift-new-' + RUN[-6:]
    env = {item['name']: item.get('value') for item in get('deployment', 'database-hook', 'infra-runners')['spec']['template']['spec']['containers'][0]['env']}
    try:
        database(name)
        ready(name)
        plan = check(name, 'c1')
        assert plan['drift'] is False, plan
        print('1: drift check of an unchanged database reports no drift', flush=True)

        role = credentials(name)['username']
        sql(f'ALTER ROLE "{role}" CONNECTION LIMIT 7;')
        plan = check(name, 'c2')
        assert plan['drift'] is True and plan['changes']['change'] >= 1, plan
        assert sql(f"SELECT rolconnlimit FROM pg_roles WHERE rolname='{role}';") == '7', 'drift check repaired infrastructure'
        print('2: manual change detected as drift; nothing was changed', flush=True)

        applied = get('databases.platform.example.com', name)['status']['lastAppliedRevision']
        before = jobs(name)
        hook_env(MODULE_ROLLOUT='manual', MODULE_REVISION=OLDER)
        time.sleep(20)
        assert jobs(name) == before and get('databases.platform.example.com', name)['status']['lastAppliedRevision'] == applied, 'manual rollout re-applied an existing resource'
        database(fresh)
        assert ready(fresh)['status']['lastAppliedRevision'] == OLDER, 'new resource did not get the configured revision'
        subprocess.run(['.local/bin/admin', '--kubeconfig', CONFIG, 'upgrade-module', '--namespace', 'team-a', '--name', name], check=True, capture_output=True)
        wait(lambda: get('databases.platform.example.com', name)['status'].get('lastAppliedRevision') == OLDER and get('databases.platform.example.com', name)['status'].get('phase') == 'Ready',
             'canary upgraded', 300)
        print('3: manual rollout kept the existing resource until upgrade-module moved it', flush=True)
        print('All drift and rollout end-to-end tests passed', flush=True)
    finally:
        hook_env(MODULE_ROLLOUT=env.get('MODULE_ROLLOUT') or 'auto', MODULE_REVISION=env['MODULE_REVISION'])
        for item in (name, fresh):
            kube('delete', 'databases.platform.example.com', item, '-n', 'team-a', '--ignore-not-found', '--wait=false', check=False)
        for item in (name, fresh):
            wait(lambda item=item: get('databases.platform.example.com', item) is None, f'{item} cleanup', 300)


if __name__ == '__main__':
    main()
