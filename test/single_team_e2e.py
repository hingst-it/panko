"""Acceptance for a personal installation: one self-named team, no lab S3, upgrade without extra applies."""
import json
from pathlib import Path
import subprocess
import sys

from e2e import K, get, kube, wait

BASE = 'http://127.0.0.1:18082'
TEAM = 'home'
NAME = 'personal-db'


def curl(method, path, payload=None, expected=200):
    token = json.loads(Path('.local/team-tokens.json').read_text())[TEAM]
    command = ['curl', '--silent', '--show-error', '--max-time', '25', '--config', '-', '-X', method, BASE + path, '-w', '\n%{http_code}']
    if payload is not None:
        command += ['-H', 'Content-Type: application/json', '--data-binary', json.dumps(payload)]
    response = subprocess.run(command, input=f'header = "Authorization: Bearer {token}"\n', text=True, capture_output=True)
    assert response.returncode == 0, response.stderr
    text, code = response.stdout.rsplit('\n', 1)
    assert int(code) == expected, (method, path, code, text)
    return json.loads(text) if text else None


def jobs():
    return [j['metadata']['name'] for j in json.loads(kube('get', 'jobs', '-n', TEAM, '-o', 'json').stdout)['items']]


def ready():
    def check():
        status = curl('GET', f'/v1/teams/{TEAM}/databases/{NAME}').get('status', {})
        assert status.get('phase') != 'Failed', status
        return status.get('phase') == 'Ready'
    wait(check, 'personal database Ready', 300)


def main(step):
    forward = subprocess.Popen(K + ['-n', 'infra-gateway', 'port-forward', 'service/platform-gateway', '18082:8080'],
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        wait(lambda: subprocess.run(['curl', '--silent', '--fail', BASE + '/healthz'], stdout=subprocess.DEVNULL,
                                    stderr=subprocess.DEVNULL).returncode == 0, 'gateway port-forward', 30)
        if step == 'create':
            namespaces = {n['metadata']['name'] for n in json.loads(kube('get', 'namespaces', '-o', 'json').stdout)['items']}
            assert {TEAM, 'panko-state-' + TEAM} <= namespaces and not {'team-a', 'team-b'} & namespaces, namespaces
            assert get('statefulset', 's3', 'infra-backends') is None, 'lab S3 installed although disabled'
            curl('POST', f'/v1/teams/{TEAM}/databases', {'name': NAME, 'spec': {'owner': TEAM}}, expected=201)
            ready()
            Path('.local/single-team-jobs.json').write_text(json.dumps(jobs()))
            print('Single team: namespaces, token and Ready database without lab S3', flush=True)
        elif step == 'after-upgrade':
            ready()
            # Two hook resync periods without a new Job prove the upgrade did not re-apply.
            subprocess.run(['sleep', '20'], check=True)
            before = json.loads(Path('.local/single-team-jobs.json').read_text())
            assert jobs() == before, (before, jobs())
            ready()
            print('Single team: upgrade kept the database Ready without another apply', flush=True)
        elif step == 'delete':
            curl('DELETE', f'/v1/teams/{TEAM}/databases/{NAME}', expected=202)
            wait(lambda: get('databases.platform.example.com', NAME, TEAM) is None, 'personal database deleted', 300)
            print('Single team: deletion completed', flush=True)
    finally:
        forward.terminate()


if __name__ == '__main__':
    main(sys.argv[1])
