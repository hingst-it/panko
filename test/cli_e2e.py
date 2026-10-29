"""Owner workflow through the panko CLI only: doctor, create, wait, get, connect and delete."""
import os
import subprocess

from e2e import K, RUN, get, kube, wait

URL = 'http://127.0.0.1:18083'
CONFIG = os.environ.get('KUBECONFIG', '.local/kubeconfig')


def panko(*args, expect=0):
    result = subprocess.run(['.local/bin/panko', '--url', URL, '--team', 'team-a', '--kubeconfig', CONFIG, *args],
                            capture_output=True, text=True)
    assert result.returncode == expect, (args, result.returncode, result.stdout, result.stderr)
    return result.stdout


def login(uri):
    """Connect with the URI the CLI printed; only the test itself uses kubectl."""
    pod = 'cli-login-' + RUN[-6:]
    kube('create', '-f', '-', obj={'apiVersion': 'v1', 'kind': 'Pod', 'metadata': {'name': pod, 'namespace': 'team-a'},
         'spec': {'restartPolicy': 'Never', 'automountServiceAccountToken': False,
                  'containers': [{'name': 'psql', 'image': 'postgres:17.11-alpine', 'env': [{'name': 'URI', 'value': uri}],
                                  'command': ['sh', '-c', 'psql "$URI" -v ON_ERROR_STOP=1 -Atc "SELECT current_user"']}]}})
    try:
        wait(lambda: get('pod', pod)['status'].get('phase') in ('Succeeded', 'Failed'), 'login pod', 120)
        assert get('pod', pod)['status']['phase'] == 'Succeeded', kube('logs', pod, '-n', 'team-a', check=False).stdout
    finally:
        kube('delete', 'pod', pod, '-n', 'team-a', '--wait=false', check=False)


def main():
    name = 'cli-' + RUN[-6:]
    forward = subprocess.Popen(K + ['-n', 'infra-gateway', 'port-forward', 'service/platform-gateway', '18083:8080'],
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        wait(lambda: subprocess.run(['curl', '--silent', '--fail', URL + '/healthz'], stdout=subprocess.DEVNULL,
                                    stderr=subprocess.DEVNULL).returncode == 0, 'gateway port-forward', 30)
        doctor = panko('doctor')
        assert 'fail' not in doctor, doctor
        panko('create', name, '--extensions', 'pgcrypto')
        panko('wait', name, '--timeout', '5m')
        summary = panko('get', name)
        assert 'Ready' in summary and 'state backend: kubernetes' in summary, summary
        uri = panko('connect', name).strip()
        assert uri.startswith('postgresql://'), 'connect did not print a URI'
        login(uri)
        panko('delete', name, '--wait', '--timeout', '5m')
        assert get('databases.platform.example.com', name) is None
        print('CLI: doctor, create, wait, get, connect with a working login and delete passed', flush=True)
    finally:
        kube('delete', 'databases.platform.example.com', name, '-n', 'team-a', '--ignore-not-found', '--wait=false', check=False)
        forward.terminate()


if __name__ == '__main__':
    main()
