"""Provider mirror: jobs install providers through the in-cluster cache and fall back when it is down."""
import json
import time

from e2e import RUN, get, kube, ready, wait

NAME = 'mirror-' + RUN[-6:]


def database(name):
    kube('create', '-f', '-', obj={'apiVersion': 'platform.example.com/v1', 'kind': 'Database',
         'metadata': {'name': name, 'namespace': 'team-a', 'labels': {'test-run': RUN}},
         'spec': {'owner': 'team-a', 'extensions': [], 'connectionLimit': 20}})


def runner_log(name):
    uid = get('db', name)['metadata']['uid']
    jobs = json.loads(kube('get', 'jobs', '-n', 'team-a', '-l', 'platform.example.com/uid=' + uid, '-o', 'json').stdout)['items']
    return ''.join(kube('logs', 'job/' + j['metadata']['name'], '-n', 'team-a', '-c', 'tofu', check=False).stdout for j in jobs)


def mirror(replicas):
    kube('scale', 'deploy/provider-mirror', '-n', 'infra-runners', f'--replicas={replicas}')
    if replicas:
        kube('rollout', 'status', 'deploy/provider-mirror', '-n', 'infra-runners', '--timeout=120s')
    else:
        wait(lambda: not json.loads(kube('get', 'pods', '-n', 'infra-runners', '-l', 'app=provider-mirror', '-o', 'json').stdout)['items'],
             'mirror stopped', 120)


def main():
    names = [NAME, NAME + '-direct']
    try:
        # 1: earlier suites already ran jobs through the mirror; its cache holds verified archives.
        database(NAME)
        ready(NAME)
        log = runner_log(NAME)
        assert 'provider_mirror_unavailable' not in log, 'runner did not use the reachable mirror'
        archives = kube('exec', '-n', 'infra-runners', 'deploy/provider-mirror', '--', 'find', '/cache', '-name', '*.zip').stdout.split()
        assert any('hashicorp/kubernetes' in a for a in archives) and any('cyrilgdn/postgresql' in a for a in archives), archives
        print('1: providers installed through the mirror; verified archives cached', flush=True)

        # 2: with the mirror down, a new resource still provisions through direct registry access.
        mirror(0)
        database(names[1])
        ready(names[1])
        assert 'provider_mirror_unavailable' in runner_log(names[1]), 'fallback not reported'
        print('2: mirror down: runner fell back to the registry and reported it', flush=True)
        print('All provider mirror end-to-end tests passed', flush=True)
    finally:
        mirror(1)
        for name in names:
            kube('delete', 'db', name, '-n', 'team-a', '--ignore-not-found', '--wait=false', check=False)
        for name in names:
            wait(lambda name=name: get('db', name) is None, f'{name} cleanup', 300)
        time.sleep(1)


if __name__ == '__main__':
    main()
