"""Runner egress rules: Jobs still provision through the mirrors, everything else is unreachable."""
import json
import os
import subprocess

from e2e import RUN, get, kube, ready, wait

CONFIG = os.environ.get('KUBECONFIG', '.local/kubeconfig')
PROBE = 'egress-probe-' + RUN[-6:]


def probe():
    """Start a long-lived pod the runner policy selects. Policy controllers such as kube-router install a
    new pod's rules asynchronously, so the probe waits until the gateway is blocked before judging."""
    kube('create', '-f', '-', obj={'apiVersion': 'v1', 'kind': 'Pod',
         'metadata': {'name': PROBE, 'namespace': 'team-a', 'labels': {'platform.example.com/uid': 'egress-probe'}},
         'spec': {'restartPolicy': 'Never', 'automountServiceAccountToken': False, 'terminationGracePeriodSeconds': 0,
                  'containers': [{'name': 'probe', 'image': 'postgres:17.11-alpine', 'command': ['sleep', '600']}]}})
    kube('wait', '--for=condition=Ready', 'pod/' + PROBE, '-n', 'team-a', '--timeout=120s')
    wait(lambda: not reachable('platform-gateway.infra-gateway.svc', 8080), 'gateway reachable from runner pods', 90)


def reachable(host, port):
    """Run nc in the probe pod; returns True if the connection opened."""
    return kube('exec', '-n', 'team-a', PROBE, '--', 'nc', '-z', '-w', '4', host, str(port), check=False).returncode == 0


def main():
    name = 'egress-' + RUN[-6:]
    policies = subprocess.run(['.local/bin/admin', '--kubeconfig', CONFIG, 'egress-policies'], check=True, capture_output=True, text=True).stdout
    try:
        kube('apply', '-f', '-', obj=json.loads(policies))
        kube('create', '-f', '-', obj={'apiVersion': 'platform.example.com/v1', 'kind': 'Database',
             'metadata': {'name': name, 'namespace': 'team-a', 'labels': {'test-run': RUN}},
             'spec': {'owner': 'team-a', 'extensions': [], 'connectionLimit': 20}})
        ready(name)
        print('1: runner Job provisioned through the mirrors under the egress policy', flush=True)
        probe()
        assert reachable('postgres.infra-backends.svc', 5432), 'declared PostgreSQL target blocked'
        assert not reachable('1.1.1.1', 443), 'internet reachable from runner pods'
        print('2: declared target reachable; gateway and internet blocked', flush=True)
        print('All egress end-to-end tests passed', flush=True)
    finally:
        kube('delete', 'pod', PROBE, '-n', 'team-a', '--ignore-not-found', '--wait=false', check=False)
        kube('delete', 'db', name, '-n', 'team-a', '--ignore-not-found', '--wait=false', check=False)
        wait(lambda: get('db', name) is None, f'{name} cleanup', 300)
        for team in subprocess.run(['.local/bin/admin', 'teams'], capture_output=True, text=True).stdout.split():
            kube('delete', 'networkpolicy', 'panko-runner-egress', '-n', team, '--ignore-not-found', check=False)


if __name__ == '__main__':
    main()
