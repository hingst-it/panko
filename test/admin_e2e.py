"""Exercise real migration refusal, conflicts and recovery using isolated test Secrets."""
import base64
import json
import os
from pathlib import Path
import subprocess
import time

from e2e import get, kube

CONFIG = os.environ.get('KUBECONFIG', '.local/kubeconfig')
ADMIN = str(Path('.local/bin/admin').resolve())
RUN = str(time.time_ns())
NAMES = [f'tfstate-default-team-a-migration-{RUN}-{suffix}' for suffix in ('a', 'b')]
LEASE = 'migration-test-' + RUN
BACKUP = Path('.local') / ('migration-test-backups-' + RUN)


def admin(command, expected=0):
    result = subprocess.run([ADMIN, '--kubeconfig', CONFIG, '--backup-dir', str(BACKUP), command],
                            text=True, capture_output=True)
    assert result.returncode == expected, (command, result.stdout, result.stderr)
    return result


def secret(name, namespace, value):
    return {'apiVersion': 'v1', 'kind': 'Secret', 'metadata': {'name': name, 'namespace': namespace,
            'labels': {'panko-test': RUN}}, 'type': 'Opaque',
            'data': {'tfstate': base64.b64encode(value.encode()).decode()}}


def main():
    for team in ('team-a', 'team-b'):
        assert not json.loads(kube('get', 'databases.platform.example.com', '-n', team, '-o', 'json').stdout)['items'], 'Use a quiescent lab'
    legacy = json.loads(kube('get', 'secrets', '-n', 'infra-runners', '-o', 'json').stdout)['items']
    assert not any(s['metadata']['name'].startswith('tfstate-') for s in legacy), 'Legacy state must not be a test fixture'
    assert get('secret', 'gateway-auth', 'infra-runners') is None, 'Legacy auth must not be a test fixture'
    gateway = get('secret', 'gateway-auth', 'infra-gateway')
    old_gateway = {'apiVersion': 'v1', 'kind': 'Secret', 'metadata': {'name': 'gateway-auth', 'namespace': 'infra-runners'},
                   'type': gateway['type'], 'data': gateway['data']}
    replicas = get('deployment', 'database-hook', 'infra-runners')['spec']['replicas']
    # Migration also pauses the gateway in infra-gateway; later tests need it back.
    gateway_replicas = get('deployment', 'platform-gateway', 'infra-gateway')['spec']['replicas']
    try:
        for name in NAMES:
            kube('create', '-f', '-', obj=secret(name, 'infra-runners', 'state-' + name))
        kube('create', '-f', '-', obj=old_gateway)
        assert 'legacy state' in admin('preflight', expected=1).stderr.lower()
        kube('create', '-f', '-', obj={'apiVersion': 'coordination.k8s.io/v1', 'kind': 'Lease',
             'metadata': {'name': LEASE, 'namespace': 'infra-runners'}, 'spec': {'holderIdentity': 'migration-test'}})
        assert 'lease is held' in admin('migrate-state', expected=1).stderr.lower()
        kube('delete', 'lease', LEASE, '-n', 'infra-runners')
        kube('create', '-f', '-', obj=secret(NAMES[1], 'panko-state-team-a', 'conflicting-state'))
        assert 'differs' in admin('migrate-state', expected=1).stderr.lower()
        assert all(get('secret', name, 'infra-runners') is not None for name in NAMES), 'Source removed before all copies verified'
        kube('delete', 'secret', NAMES[1], '-n', 'panko-state-team-a')
        admin('migrate-state')
        for name in NAMES:
            assert get('secret', name, 'infra-runners') is None
            target = get('secret', name, 'panko-state-team-a')
            assert target['data'] == secret(name, '', 'state-' + name)['data']
            assert target['metadata']['labels']['panko-test'] == RUN
        assert get('secret', 'gateway-auth', 'infra-gateway')['data'] == gateway['data'], 'Tokens changed'
        backups = list(BACKUP.glob('state-backup-*.json'))
        assert len(backups) >= 2 and all(p.stat().st_mode & 0o777 == 0o600 for p in backups)
        admin('preflight')
        print('Admin migration: held locks and destination conflicts refused; sources retained; resume, auth preservation and private backups passed', flush=True)
    finally:
        for namespace in ('infra-runners', 'panko-state-team-a'):
            for name in NAMES:
                kube('delete', 'secret', name, '-n', namespace, '--ignore-not-found', check=False)
        kube('delete', 'lease', LEASE, '-n', 'infra-runners', '--ignore-not-found', check=False)
        kube('delete', 'secret', 'gateway-auth', '-n', 'infra-runners', '--ignore-not-found', check=False)
        kube('scale', 'deploy/database-hook', '-n', 'infra-runners', '--replicas=' + str(replicas))
        kube('rollout', 'status', 'deploy/database-hook', '-n', 'infra-runners', '--timeout=120s')
        kube('scale', 'deploy/platform-gateway', '-n', 'infra-gateway', '--replicas=' + str(gateway_replicas))
        kube('rollout', 'status', 'deploy/platform-gateway', '-n', 'infra-gateway', '--timeout=120s')


if __name__ == '__main__':
    main()
