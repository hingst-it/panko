"""Check the running hook's Prometheus endpoint and structured lifecycle logs."""
import json

from e2e import kube

metrics = kube('get', '--raw', '/api/v1/namespaces/infra-runners/services/database-hook:8080/proxy/metrics').stdout
for action in ('apply', 'destroy'):
    assert f'panko_execution_total{{action="{action}",outcome="success"}}' in metrics
    assert f'panko_execution_duration_seconds_count{{action="{action}"}}' in metrics
assert 'panko_blocked_deletions 0' in metrics
assert 'uid=' not in metrics and 'namespace=' not in metrics
logs = kube('logs', '-n', 'infra-runners', 'deploy/database-hook', '--tail=2000').stdout
# The controller pod also carries the embedded Metacontroller's JSON logs (msg/ts); the hook's
# reconciliation events are the lines with an event key.
lines = [json.loads(line) for line in logs.splitlines() if line.startswith('{')]
events = [line for line in lines if 'event' in line]
assert events, 'No structured reconciliation events'
assert all({'uid', 'namespace', 'name', 'generation', 'action', 'job'} <= event.keys() for event in events)
assert not any({'spec', 'password', 'token'} & line.keys() for line in lines)
# Every long-running role serves metrics: the endpoint and the provider mirror through their TLS
# Services, the embedded Metacontroller next to the hook.
for service, metric in (('https:runner-endpoint:443', 'panko_endpoint_requests_total{route="healthz",code="2xx"}'),
                        ('https:provider-mirror:8443', 'panko_mirror_requests_total{route="healthz",code="2xx"}'),
                        ('database-hook:9999', 'controller_runtime_reconcile_total')):
    text = kube('get', '--raw', f'/api/v1/namespaces/infra-runners/services/{service}/proxy/metrics').stdout
    assert metric in text, f'{service} lacks {metric}'
print('Observability: live hook, endpoint, mirror and Metacontroller metrics and structured lifecycle logs verified', flush=True)
