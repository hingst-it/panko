"""Check effective Kubernetes permissions using administrator impersonation."""
from e2e import kube


def allowed(subject, verb, resource, namespace):
    extra = []
    if resource.endswith('/status'):
        resource = resource.removesuffix('/status')
        extra = ['--subresource=status']
    response = kube('auth', 'can-i', verb, resource, '-n', namespace, '--as=' + subject, *extra, check=False)
    if response.returncode not in (0, 1):
        raise AssertionError(response.stderr)
    return response.stdout.strip() == 'yes'


def main():
    for team, other in (('team-a', 'team-b'), ('team-b', 'team-a')):
        subject = f'system:serviceaccount:{team}:tofu-runner'
        for verb in ('get', 'list', 'create', 'update', 'patch', 'delete'):
            for resource in ('secrets', 'leases.coordination.k8s.io'):
                assert allowed(subject, verb, resource, 'panko-state-' + team), (team, verb, resource)
                assert not allowed(subject, verb, resource, 'panko-state-' + other), (team, verb, resource)
                assert not allowed(subject, verb, resource, 'infra-runners'), (team, verb, resource)
                assert not allowed(subject, verb, resource, 'infra-gateway'), (team, verb, resource)
        assert not allowed(subject, 'get', 'secrets/gateway-auth', 'infra-gateway')
        assert not allowed(subject, 'get', 'secrets/gateway-auth', 'infra-runners')
        assert allowed(subject, 'get', 'secrets/postgres-admin', 'infra-backends')
        assert not allowed(subject, 'list', 'secrets', 'infra-backends')
    gateway = 'system:serviceaccount:infra-gateway:platform-gateway'
    for team in ('team-a', 'team-b'):
        assert allowed(gateway, 'create', 'databases.platform.example.com', team)
        for resource in ('secrets', 'jobs.batch', 'databases.platform.example.com/status'):
            assert not allowed(gateway, 'patch', resource, team), (team, resource)
    print('RBAC: own-team state allowed; foreign state, gateway tokens and privileged gateway writes denied', flush=True)


if __name__ == '__main__':
    main()
