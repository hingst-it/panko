"""Failure reporting per stage and explicit retry through the gateway, against a real lab."""
import base64
import json
from pathlib import Path
import subprocess
import time

from e2e import K, RUN, credentials, get, kube, wait

BASE = 'http://127.0.0.1:18081'
TOKENS = json.loads(Path('.local/team-tokens.json').read_text())


def curl(method, path, expected):
    # The token travels on stdin, never in argv.
    command = ['curl', '--silent', '--show-error', '--max-time', '25', '--config', '-', '-X', method, BASE + path, '-w', '\n%{http_code}']
    response = subprocess.run(command, input=f'header = "Authorization: Bearer {TOKENS["team-a"]}"\n', text=True, capture_output=True)
    assert response.returncode == 0, response.stderr
    text, code = response.stdout.rsplit('\n', 1)
    assert int(code) == expected, (method, path, code, text)
    return json.loads(text) if text else None


def database(name):
    kube('create', '-f', '-', obj={'apiVersion': 'platform.example.com/v1', 'kind': 'Database',
         'metadata': {'name': name, 'namespace': 'team-a', 'labels': {'test-run': RUN}},
         'spec': {'owner': 'team-a', 'extensions': [], 'connectionLimit': 20}})


def failed(name, code):
    def check():
        status = get('db', name).get('status', {})
        execution = status.get('lastExecution', {})
        return status.get('phase') == 'Failed' and execution.get('result') == 'Failed' and execution
    execution = wait(check, f'{name} failed', 300)
    assert execution['code'] == code, execution
    message = get('db', name)['status']['message']
    assert f'({code})' in message and 'retry action' in message, message
    return execution


def ready_with(name, revision):
    """Wait until the hook reports the given module revision as Ready; earlier failures may still show."""
    def check():
        status = get('db', name).get('status', {})
        if status.get('observedRevision') != revision:
            return False
        assert status.get('phase') != 'Failed', status.get('message')
        return status.get('phase') == 'Ready'
    wait(check, f'{name} Ready with revision {revision}', 300)


def ready_after(name, attempt):
    """Wait for a newer attempt than the failed one to finish Ready; the old failure may still show."""
    def check():
        status = get('db', name).get('status', {})
        execution = status.get('lastExecution', {})
        if execution.get('attempt', 0) <= attempt:
            return False
        assert status.get('phase') != 'Failed', status.get('message')
        return status.get('phase') == 'Ready' and execution
    return wait(check, f'{name} Ready after attempt {attempt}', 300)


def hook_env(**values):
    kube('set', 'env', '-n', 'infra-runners', 'deploy/database-hook', *[f'{k}={v}' for k, v in values.items()])
    kube('rollout', 'status', '-n', 'infra-runners', 'deploy/database-hook', '--timeout=120s')


def main():
    forward = subprocess.Popen(K + ['-n', 'infra-gateway', 'port-forward', 'service/platform-gateway', '18081:8080'],
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    names = [f'fail-{kind}-{RUN[-6:]}' for kind in ('provider', 'module', 'engine')]
    deployment = get('deployment', 'database-hook', 'infra-runners')['spec']['template']['spec']['containers'][0]['env']
    original = {item['name']: item['value'] for item in deployment if 'value' in item}
    admin = get('secret', 'postgres-admin', 'infra-backends')
    try:
        wait(lambda: subprocess.run(['curl', '--silent', '--fail', BASE + '/healthz'], stdout=subprocess.DEVNULL,
                                    stderr=subprocess.DEVNULL).returncode == 0, 'gateway port-forward', 30)

        # 1: provider failure, reported after the Job is gone, then repaired with the retry action.
        provider = names[0]
        wrong = base64.b64encode(b'not-the-password').decode()
        kube('patch', 'secret', 'postgres-admin', '-n', 'infra-backends', '--type=merge', '-p', json.dumps({'data': {'password': wrong}}))
        database(provider)
        first = failed(provider, 'provider')
        kube('delete', 'job', first['job'], '-n', 'team-a', '--wait=true')
        time.sleep(6)
        assert get('db', provider)['status']['lastExecution']['code'] == 'provider', 'execution record lost with the Job'
        kube('patch', 'secret', 'postgres-admin', '-n', 'infra-backends', '--type=merge', '-p', json.dumps({'data': {'password': admin['data']['password']}}))
        curl('POST', f'/v1/teams/team-a/databases/{provider}/retry', 202)
        execution = ready_after(provider, first['attempt'])
        assert execution['result'] == 'Succeeded' and execution['attempt'] == first['attempt'] + 1 and execution.get('retryToken'), execution
        assert execution.get('changes', {}).get('add', 0) > 0, execution
        assert credentials(provider)['database']
        curl('POST', f'/v1/teams/team-a/databases/{provider}/retry', 409)
        print('1: provider failure reported after Job expiry; gateway retry continued the attempts and succeeded', flush=True)

        # 2: module fetch failure from an unknown pinned revision.
        hook_env(MODULE_REVISION='0' * 40)
        database(names[1])
        failed(names[1], 'module_fetch')
        hook_env(MODULE_REVISION=original['MODULE_REVISION'])
        ready_with(names[1], original['MODULE_REVISION'])
        print('2: module fetch failure reported; restored revision applied', flush=True)

        # 3: engine preparation failure from a wrong binary path.
        hook_env(ENGINE_BINARY='/usr/local/bin/missing-tofu')
        database(names[2])
        engine = failed(names[2], 'engine_copy')
        hook_env(ENGINE_BINARY=original['ENGINE_BINARY'])
        curl('POST', f'/v1/teams/team-a/databases/{names[2]}/retry', 202)
        ready_after(names[2], engine['attempt'])
        print('3: engine failure reported; retry after restoring the engine succeeded', flush=True)
        print('All failure reporting end-to-end tests passed', flush=True)
    finally:
        kube('patch', 'secret', 'postgres-admin', '-n', 'infra-backends', '--type=merge', '-p', json.dumps({'data': {'password': admin['data']['password']}}), check=False)
        hook_env(MODULE_REVISION=original['MODULE_REVISION'], ENGINE_BINARY=original['ENGINE_BINARY'])
        for name in names:
            kube('delete', 'db', name, '-n', 'team-a', '--ignore-not-found', '--wait=false', check=False)
        for name in names:
            wait(lambda name=name: get('db', name) is None, f'{name} cleanup', 300)
        forward.terminate()


if __name__ == '__main__':
    main()
