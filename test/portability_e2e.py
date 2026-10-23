"""Explicit upgrade/rollback checks against locally retained release images.

Run only in the dedicated lab; each mode creates and cleans its own Database.
"""
import argparse
import copy
import json
import time

from e2e import create, credentials, delete_and_check, get, kube, ready, sql, wait


def hook_image(image):
    kube('set', 'image', '-n', 'infra-runners', 'deploy/database-hook', 'hook=' + image)
    kube('scale', '-n', 'infra-runners', 'deploy/database-hook', '--replicas=1')
    kube('rollout', 'status', '-n', 'infra-runners', 'deploy/database-hook', '--timeout=120s')


def runner_image(image):
    kube('set', 'env', '-n', 'infra-runners', 'deploy/database-hook', 'RUNNER_IMAGE=' + image)
    kube('rollout', 'status', '-n', 'infra-runners', 'deploy/database-hook', '--timeout=120s')


def jobs(uid):
    return json.loads(kube('get', 'jobs', '-n', 'team-a', '-l',
                          'platform.example.com/uid=' + uid, '-o', 'json').stdout)['items']


def hook_transition(old_image, new_image):
    name = 'hook-upgrade-' + str(time.time_ns())
    quota = 'upgrade-quota-' + str(time.time_ns())
    kube('create', '-f', '-', obj={'apiVersion': 'v1', 'kind': 'ResourceQuota',
         'metadata': {'name': quota, 'namespace': 'team-a'}, 'spec': {'hard': {'pods': '0'}}})
    try:
        create(name)
        parent = get('db', name)
        child = wait(lambda: next(iter(jobs(parent['metadata']['uid'])), None), 'Python Job scheduled')
        kube('scale', '-n', 'infra-runners', 'deploy/database-hook', '--replicas=0')
        kube('rollout', 'status', '-n', 'infra-runners', 'deploy/database-hook', '--timeout=120s')
        hook_pods = json.loads(kube('get', 'pods', '-n', 'infra-runners', '-l',
                                    'app=database-hook', '-o', 'json').stdout)['items']
        if hook_pods:
            kube('wait', '--for=delete', 'pods', '-n', 'infra-runners', '-l', 'app=database-hook', '--timeout=120s')
        # Keep an old runner active long enough to verify multiple hook rollouts.
        replacement = copy.deepcopy(child)
        replacement['metadata'] = {k: child['metadata'][k] for k in
                                   ('name', 'namespace', 'labels', 'annotations', 'ownerReferences')
                                   if k in child['metadata']}
        replacement.pop('status', None)
        replacement['spec'].pop('selector', None)
        replacement['spec'].pop('manualSelector', None)
        replacement['spec']['template'].pop('metadata', None)
        container = replacement['spec']['template']['spec']['containers'][0]
        container['command'] = ['/bin/sh', '-c', 'sleep 45; exec python3 /opt/entrypoint.py "$@"', 'upgrade']
        kube('delete', 'job', child['metadata']['name'], '-n', 'team-a', '--wait=true')
        kube('create', '-f', '-', obj=replacement)
        kube('delete', 'resourcequota', quota, '-n', 'team-a')
        def running():
            pods = json.loads(kube('get', 'pods', '-n', 'team-a', '-l',
                                  'job-name=' + child['metadata']['name'], '-o', 'json').stdout)['items']
            return any(p.get('status', {}).get('phase') == 'Running' for p in pods)
        wait(running, 'old Python runner active')
        original = get('job', child['metadata']['name'])
        for image in (new_image, old_image, new_image):
            hook_image(image)
            retained = get('job', child['metadata']['name'])
            assert retained['metadata']['uid'] == original['metadata']['uid'], 'Running Job replaced'
            assert retained['spec']['template'] == original['spec']['template'], 'Pod template changed'
        ready(name)
        delete_and_check(name, 'team-a', credentials(name))
        print('Hook upgrade/rollback: active Python Job retained without replacement; lifecycle completed', flush=True)
    finally:
        kube('delete', 'resourcequota', quota, '-n', 'team-a', '--ignore-not-found', check=False)
        hook_image(new_image)
        kube('delete', 'db', name, '-n', 'team-a', '--ignore-not-found', '--wait=false', check=False)


def runner_transition(old_image, new_image):
    name = 'runner-upgrade-' + str(time.time_ns())
    try:
        runner_image(old_image)
        create(name)
        ready(name)
        original = credentials(name)
        for image, limit in ((new_image, 35), (old_image, 36), (new_image, 37)):
            runner_image(image)
            changed = json.loads(kube('patch', 'db', name, '-n', 'team-a', '--type=merge', '-p',
                                     json.dumps({'spec': {'connectionLimit': limit}}), '-o', 'json').stdout)
            current = ready(name, generation=changed['metadata']['generation'])
            assert credentials(name) == original, 'Connection identity or password changed across language switch'
            assert sql(f"SELECT datconnlimit FROM pg_database WHERE datname='{original['database']}';") == str(limit)
            assert any(j['spec']['template']['spec']['containers'][0]['image'] == image
                       and j['metadata']['annotations']['platform.example.com/hash'] == current['status']['lastAppliedHash']
                       for j in jobs(current['metadata']['uid']))
        delete_and_check(name, 'team-a', original)
        print('Runner upgrade/rollback: Python state reused by Go, Python and Go; credentials retained; Go destroy passed', flush=True)
    finally:
        runner_image(new_image)
        kube('delete', 'db', name, '-n', 'team-a', '--ignore-not-found', '--wait=false', check=False)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('component', choices=['hook', 'runner'])
    parser.add_argument('--old-image', required=True)
    parser.add_argument('--new-image', required=True)
    args = parser.parse_args()
    if args.component == 'hook':
        hook_transition(args.old_image, args.new_image)
    else:
        runner_transition(args.old_image, args.new_image)
