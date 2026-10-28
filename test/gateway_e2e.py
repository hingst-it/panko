"""Use curl against a real forwarded gateway, including API-server CEL errors."""
import json
from pathlib import Path
import subprocess
import time

from e2e import kube, wait, credentials, sql, connect, get

BASE = 'http://127.0.0.1:18080'
TOKENS = json.loads(Path('.local/team-tokens.json').read_text())
RUN = 'gateway-' + str(int(time.time()))


def curl(method, path, payload=None, team='team-a', expected=200, token=True):
    # Curl reads the Authorization header from stdin, keeping tokens out of argv/logs.
    command = ['curl', '--silent', '--show-error', '--max-time', '25', '--config', '-',
               '-X', method, BASE + path, '-w', '\n%{http_code}']
    if payload is not None:
        command += ['-H', 'Content-Type: application/json', '--data-binary', json.dumps(payload)]
    config = f'header = "Authorization: Bearer {TOKENS[team]}"\n' if token else ''
    response = subprocess.run(command, input=config, text=True, capture_output=True)
    assert response.returncode == 0, response.stderr
    text, code = response.stdout.rsplit('\n', 1)
    assert int(code) == expected, (method, path, code, text)
    return json.loads(text) if text else None


# Wait for port forwarding without exposing credentials.
wait(lambda: subprocess.run(['curl', '--silent', '--fail', BASE + '/healthz'],
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0,
     'gateway port-forward', seconds=30)
path = '/v1/teams/team-a/databases'
name = RUN + '-orders'
created = False
try:
    curl('GET', path, expected=401, token=False)
    curl('GET', '/v1/teams/team-b/databases', expected=403)
    curl('GET', '/v1/teams/team-a/secrets', expected=404)
    cr = curl('POST', path, {'name': name, 'spec': {'owner': 'team-a', 'extensions': ['pgcrypto']}} , expected=201)
    created = True
    assert set(cr).issubset({'name', 'spec', 'status'}) and cr['spec']['connectionLimit'] == 20
    def is_ready():
        result = curl('GET', path + '/' + name)
        assert result.get('status', {}).get('phase') != 'Failed', result
        return result if result.get('status', {}).get('phase') == 'Ready' else None
    ready = wait(is_ready, 'gateway database Ready')
    assert any(item['name'] == name for item in curl('GET', path))
    connect(name)
    changed = curl('PATCH', path + '/' + name, {'spec': {'connectionLimit': 27}})
    wait(lambda: (result := is_ready()) and result['status']['lastAppliedHash'] != ready['status']['lastAppliedHash'],
         'gateway patched database Ready')
    creds = credentials(name)
    assert sql(f"SELECT datconnlimit FROM pg_database WHERE datname='{creds['database']}';") == '27'
    for payload, marker in [({'spec': {'connectionLimit': 0}}, 'spec.connectionLimit'),
                            ({'spec': {'owner': 'other'}}, 'spec'),
                            ({'spec': {'extensions': ['invalid']}}, 'spec.extensions')]:
        failed = curl('PATCH', path + '/' + name, payload, expected=422)
        assert any(marker in cause['field'] for cause in failed['errors']), failed
    curl('PATCH', path + '/' + name, {'metadata': {'namespace': 'team-b'}}, expected=422)
    curl('DELETE', path + '/' + name, expected=202)
    wait(lambda: get('databases.platform.example.com', name) is None, 'gateway database deleted')
    assert get('secret', 'tfstate-default-team-a-' + name, 'panko-state-team-a') is None
    assert sql(f"SELECT count(*) FROM pg_database WHERE datname='{creds['database']}';") == '0'
    assert sql(f"SELECT count(*) FROM pg_roles WHERE rolname='{creds['username']}';") == '0'
    print('Gateway curl CRUD, Ready, real login, validation paths, isolation and deletion passed', flush=True)
finally:
    if created:
        kube('delete', 'databases.platform.example.com', name, '-n', 'team-a', '--ignore-not-found', '--wait=false', check=False)

# Add a new resource after the gateway is running: no image rebuild or restart.
initial = curl('GET', '/openapi.json', token=False)
assert '/v1/teams/{team}/widgets' not in initial['paths']
kube('apply', '-f', 'test/dummy-crd.yaml')
kube('wait', '--for=condition=Established', 'crd/widgets.platform.example.com', '--timeout=60s')
try:
    def widget_document():
        document = curl('GET', '/openapi.json', token=False)
        return document if '/v1/teams/{team}/widgets' in document['paths'] else None
    document = wait(widget_document, 'new CRD visible after discovery cache expiry', seconds=15)
    assert document['openapi'] == '3.1.0'
    assert '/v1/teams/{team}/widgets' in document['paths']
    widget = document['components']['schemas']['platform_example_com_v1_WidgetCreate']
    assert widget['properties']['spec']['properties']['color']['description'] == 'Widget color, documented only in the CRD.'
    # Publishing schemas needs no write privileges. Explicitly synchronize resource RBAC as admin.
    subprocess.run(['make', 'gateway-rbac'], check=True, stdout=subprocess.DEVNULL)
    curl('POST', '/v1/teams/team-a/widgets', {'name': RUN, 'spec': {'color': 'blue'}}, expected=201)
    assert curl('GET', '/v1/teams/team-a/widgets/' + RUN)['spec']['color'] == 'blue'
    bad = curl('PATCH', '/v1/teams/team-a/widgets/' + RUN, {'spec': {'color': 'green'}}, expected=422)
    assert any('spec.color' in e['field'] for e in bad['errors'])
    curl('DELETE', '/v1/teams/team-a/widgets/' + RUN, expected=202)
    Path('.local/openapi.json').write_text(json.dumps(document, indent=2))
    html = subprocess.check_output(['curl', '-fsS', BASE + '/docs']).decode()
    assert 'swagger-ui' in html
    metrics = subprocess.check_output(['curl', '-fsS', BASE + '/metrics']).decode()
    assert 'panko_discovery_cache_requests_total{result="hit"}' in metrics
    assert 'panko_discovery_cache_requests_total{result="miss"}' in metrics
    assert not any(token in metrics for token in TOKENS.values())
    print('New CRD appeared automatically with its own field documentation; generic CRUD passed', flush=True)
finally:
    kube('delete', 'crd', 'widgets.platform.example.com', '--ignore-not-found')
    subprocess.run(['make', 'gateway-rbac'], check=True, stdout=subprocess.DEVNULL)
