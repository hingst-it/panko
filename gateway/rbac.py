"""Generate least-privilege RBAC from currently exposed CRDs, run by an administrator."""
import json
import subprocess
from schema import resources

crds = json.loads(subprocess.check_output(['kubectl', 'get', 'crds', '-o', 'json']))['items']
discovered = resources(crds)
items = []
role_ref = {'apiGroup': 'rbac.authorization.k8s.io', 'kind': 'ClusterRole', 'name': 'platform-gateway-discovery'}
subject = {'kind': 'ServiceAccount', 'name': 'platform-gateway', 'namespace': 'infra-runners'}
items.append({'apiVersion': 'rbac.authorization.k8s.io/v1', 'kind': 'ClusterRole',
              'metadata': {'name': role_ref['name']}, 'rules': [
                  {'apiGroups': ['apiextensions.k8s.io'], 'resources': ['customresourcedefinitions'], 'verbs': ['get', 'list']}]})
items.append({'apiVersion': 'rbac.authorization.k8s.io/v1', 'kind': 'ClusterRoleBinding',
              'metadata': {'name': role_ref['name']}, 'roleRef': role_ref, 'subjects': [subject]})
rules = [{'apiGroups': [group], 'resources': sorted({r['plural'] for r in discovered.values() if r['group'] == group}),
          'verbs': ['get', 'list', 'create', 'patch', 'delete']} for group in sorted({r['group'] for r in discovered.values()})]
for team in ['team-a', 'team-b']:
    items.append({'apiVersion': 'rbac.authorization.k8s.io/v1', 'kind': 'Role',
                  'metadata': {'name': 'platform-gateway', 'namespace': team}, 'rules': rules})
    items.append({'apiVersion': 'rbac.authorization.k8s.io/v1', 'kind': 'RoleBinding',
                  'metadata': {'name': 'platform-gateway', 'namespace': team},
                  'roleRef': {'apiGroup': 'rbac.authorization.k8s.io', 'kind': 'Role', 'name': 'platform-gateway'},
                  'subjects': [subject]})
print(json.dumps({'apiVersion': 'v1', 'kind': 'List', 'items': items}))
