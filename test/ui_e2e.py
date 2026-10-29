"""Embedded inspector and sanitized configuration graph against the real gateway."""
import json
from pathlib import Path
import subprocess
import tempfile
import urllib.request

from e2e import RUN, get, kube, ready, wait

BASE = 'http://127.0.0.1:18080'


def main():
    # The earlier gateway suite closes its own port-forward.
    with tempfile.TemporaryFile(mode='w+') as log:
        forward = subprocess.Popen(
            ['kubectl', '-n', 'infra-gateway', 'port-forward', '--address', '127.0.0.1',
             'service/platform-gateway', '18080:8080'], stdout=log, stderr=log)
        name = 'ui-' + RUN[-6:]
        created = False
        try:
            def reachable():
                try:
                    with urllib.request.urlopen(BASE + '/healthz', timeout=2) as response:
                        return response.status == 200
                except OSError:
                    return False
            wait(reachable, 'UI gateway port-forward', 30)
            with urllib.request.urlopen(BASE + '/ui', timeout=10) as response:
                assert response.status == 200
                csp = response.headers['Content-Security-Policy']
                assert "default-src 'self'" in csp and 'unsafe-inline' not in csp
                assert response.headers['Cache-Control'] == 'no-store'
                assert response.headers['X-Content-Type-Options'] == 'nosniff'
                assert response.headers['Referrer-Policy'] == 'no-referrer'
                assert b'/ui/app.js' in response.read()
            kube('create', '-f', '-', obj={
                'apiVersion': 'platform.example.com/v1', 'kind': 'Database',
                'metadata': {'name': name, 'namespace': 'team-a', 'labels': {'test-run': RUN}},
                'spec': {'owner': 'team-a', 'extensions': [], 'connectionLimit': 20}})
            created = True
            resource = ready(name)
            tokens = json.loads(Path('.local/team-tokens.json').read_text())
            request = urllib.request.Request(
                BASE + '/v1/teams/team-a/databases/' + name + '/graph',
                headers={'Authorization': 'Bearer ' + tokens['team-a']})
            with urllib.request.urlopen(request, timeout=10) as response:
                assert response.status == 200
                graph = json.load(response)
            assert graph['version'] == 1
            assert graph['uid'] == resource['metadata']['uid']
            assert graph['identity'] == resource['status'].get('identity', graph['uid'])
            assert graph['moduleRevision'] == resource['status']['lastAppliedRevision']
            labels = [node['label'] for node in graph['nodes']]
            assert any(label.startswith('postgresql_role.') for label in labels), labels
            assert any(label.startswith('postgresql_database.') for label in labels), labels
            assert len(labels) <= 200
            assert all('[' not in label and '(' not in label for label in labels), labels
            ids = {node['id'] for node in graph['nodes']}
            assert all(len(edge) == 2 and set(edge) <= ids for edge in graph['edges'])
            print('Embedded UI headers and Ready database resource graph passed', flush=True)
        finally:
            try:
                if created:
                    kube('delete', 'databases.platform.example.com', name, '-n', 'team-a', '--ignore-not-found', '--wait=false', check=False)
                    wait(lambda: get('databases.platform.example.com', name) is None, 'UI database cleanup', 300)
            finally:
                forward.terminate()
                forward.wait(timeout=10)


if __name__ == '__main__':
    main()
