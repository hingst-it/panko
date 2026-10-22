"""Explicit roles per trusted team namespace; no cluster-wide runner write access."""
import json

teams = ['team-a', 'team-b']
objects = []
def add(kind, name, namespace=None, **fields):
    obj = {'apiVersion': 'v1' if kind in ('Namespace', 'ServiceAccount') else 'rbac.authorization.k8s.io/v1',
           'kind': kind, 'metadata': {'name': name}, **fields}
    if namespace:
        obj['metadata']['namespace'] = namespace
    objects.append(obj)

for team in teams:
    add('Namespace', team)
    add('ServiceAccount', 'tofu-runner', team)
    add('Role', 'tofu-runner', team, rules=[
        {'apiGroups': ['platform.example.com'], 'resources': ['databases'], 'verbs': ['get']},
        {'apiGroups': [''], 'resources': ['secrets'], 'verbs': ['get', 'list', 'create', 'update', 'patch', 'delete']},
    ])
    add('RoleBinding', 'tofu-runner', team,
        roleRef={'apiGroup': 'rbac.authorization.k8s.io', 'kind': 'Role', 'name': 'tofu-runner'},
        subjects=[{'kind': 'ServiceAccount', 'name': 'tofu-runner', 'namespace': team}])
subjects = [{'kind': 'ServiceAccount', 'name': 'tofu-runner', 'namespace': t} for t in teams]
add('ClusterRole', 'tofu-schema-reader', rules=[
    {'apiGroups': ['apiextensions.k8s.io'], 'resources': ['customresourcedefinitions'], 'verbs': ['get', 'list']},
])
add('ClusterRoleBinding', 'tofu-schema-reader',
    roleRef={'apiGroup': 'rbac.authorization.k8s.io', 'kind': 'ClusterRole', 'name': 'tofu-schema-reader'}, subjects=subjects)
add('Role', 'tofu-state', 'infra-runners', rules=[
    {'apiGroups': [''], 'resources': ['secrets'], 'verbs': ['get', 'list', 'create', 'update', 'patch', 'delete']},
    {'apiGroups': ['coordination.k8s.io'], 'resources': ['leases'], 'verbs': ['get', 'list', 'create', 'update', 'patch', 'delete']},
])
add('RoleBinding', 'tofu-state', 'infra-runners',
    roleRef={'apiGroup': 'rbac.authorization.k8s.io', 'kind': 'Role', 'name': 'tofu-state'}, subjects=subjects)
add('Role', 'postgres-admin-reader', 'infra-backends', rules=[
    {'apiGroups': [''], 'resources': ['secrets'], 'resourceNames': ['postgres-admin'], 'verbs': ['get']},
])
add('RoleBinding', 'postgres-admin-reader', 'infra-backends',
    roleRef={'apiGroup': 'rbac.authorization.k8s.io', 'kind': 'Role', 'name': 'postgres-admin-reader'}, subjects=subjects)
print(json.dumps({'apiVersion': 'v1', 'kind': 'List', 'items': objects}))
