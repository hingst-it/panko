"""A real apply/destroy before introducing Metacontroller hooks."""
import json
import os
import subprocess
import time

K = ['kubectl', '--kubeconfig', os.environ.get('KUBECONFIG', '.local/kubeconfig')]

def kube(*args, obj=None):
    return subprocess.check_output(K + list(args), input=json.dumps(obj).encode() if obj else None).decode()


def job(action):
    obj = {'apiVersion': 'batch/v1', 'kind': 'Job',
           'metadata': {'name': 'runner-smoke-' + action, 'namespace': 'team-a'},
           'spec': {'backoffLimit': 0, 'activeDeadlineSeconds': 180, 'template': {'spec': {
               'serviceAccountName': 'tofu-runner', 'restartPolicy': 'Never',
               'containers': [{'name': 'tofu', 'image': 'panko-runner:dev', 'imagePullPolicy': 'Never',
                               'args': [action, 'team-a', 'runner-smoke']} ]}}}}
    kube('create', '-f', '-', obj=obj)
    for _ in range(120):
        result = json.loads(kube('get', 'job', 'runner-smoke-' + action, '-n', 'team-a', '-o', 'json'))
        status = result.get('status', {})
        if status.get('succeeded') or status.get('failed'):
            print(kube('logs', '-n', 'team-a', 'job/runner-smoke-' + action))
            assert status.get('succeeded'), status
            return
        time.sleep(2)
    raise AssertionError('runner timed out')

kube('create', '-f', '-', obj={'apiVersion': 'platform.example.com/v1', 'kind': 'Database',
     'metadata': {'name': 'runner-smoke', 'namespace': 'team-a'},
     'spec': {'owner': 'team-a', 'extensions': ['pgcrypto'], 'connectionLimit': 20}})
job('apply')
s = json.loads(kube('get', 'secret', 'runner-smoke-connection', '-n', 'team-a', '-o', 'json'))
assert s['metadata']['ownerReferences'][0]['kind'] == 'Database'
job('destroy')
assert not kube('get', 'secret', 'tfstate-default-team-a-runner-smoke', '-n', 'infra-runners', '--ignore-not-found')
kube('delete', 'db', 'runner-smoke', '-n', 'team-a')
kube('delete', 'jobs', 'runner-smoke-apply', 'runner-smoke-destroy', '-n', 'team-a')
print('Runner apply, ownerReference, destroy and state cleanup passed')
