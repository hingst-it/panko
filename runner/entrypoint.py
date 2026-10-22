"""Only CR identity crosses this boundary. Business configuration stays in HCL."""
import base64
import json
import os
from pathlib import Path
import shutil
import ssl
import subprocess
import sys
import urllib.error
import urllib.request

SA = Path('/var/run/secrets/kubernetes.io/serviceaccount')


def api(method, path):
    request = urllib.request.Request(
        'https://kubernetes.default.svc' + path, method=method,
        headers={'Authorization': 'Bearer ' + (SA / 'token').read_text().strip()})
    try:
        with urllib.request.urlopen(request, context=ssl.create_default_context(
                cafile=str(SA / 'ca.crt')), timeout=30) as response:
            return json.load(response)
    except urllib.error.HTTPError as error:
        if method == 'DELETE' and error.code == 404:
            return None
        raise


def main():
    if len(sys.argv) != 4 or sys.argv[1] not in ('apply', 'destroy'):
        sys.exit('usage: entrypoint.py apply|destroy NAMESPACE NAME')
    action, namespace, name = sys.argv[1:]
    admin = api('GET', '/api/v1/namespaces/infra-backends/secrets/postgres-admin')
    os.environ.update({
        'PGPASSWORD': base64.b64decode(admin['data']['password']).decode(),
        'KUBE_HOST': 'https://kubernetes.default.svc',
        'KUBE_TOKEN': (SA / 'token').read_text().strip(),
        'KUBE_CLUSTER_CA_CERT_DATA': (SA / 'ca.crt').read_text(),
        'TF_VAR_namespace': namespace, 'TF_VAR_name': name,
    })
    shutil.copytree('/opt/module', '/work/module', dirs_exist_ok=True)
    os.chdir('/work/module')
    subprocess.run(['tofu', 'init', '-input=false', '-lockfile=readonly',
                    '-backend-config=in_cluster_config=true',
                    '-backend-config=namespace=infra-runners',
                    f'-backend-config=secret_suffix={namespace}-{name}'], check=True)
    subprocess.run(['tofu', action, '-auto-approve', '-input=false',
                    '-lock-timeout=120s', '-no-color'], check=True)
    if action == 'apply':
        subprocess.run(['tofu', 'output', '-json'], check=True)
    else:
        # Destroy persists an empty state: remove it only after all resources are gone.
        state_name = f'tfstate-default-{namespace}-{name}'
        api('DELETE', f'/api/v1/namespaces/infra-runners/secrets/{state_name}')
        api('DELETE', f'/apis/coordination.k8s.io/v1/namespaces/infra-runners/leases/lock-{state_name}')
        print('Destroy complete; state and lock removed.', flush=True)


if __name__ == '__main__':
    main()
