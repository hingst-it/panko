"""Exercise the real API, Jobs, Kubernetes state and PostgreSQL without mocks."""
import base64
import json
import os
import subprocess
import time

K = ['kubectl', '--kubeconfig', os.environ.get('KUBECONFIG', '.local/kubeconfig')]
RUN = 'e2e-' + str(int(time.time()))
CREATED = []


def kube(*args, obj=None, check=True):
    result = subprocess.run(K + list(args), input=json.dumps(obj) if obj else None,
                            text=True, capture_output=True)
    if check and result.returncode:
        raise AssertionError(result.stderr)
    return result


def get(kind, name, namespace='team-a'):
    response = kube('get', kind, name, '-n', namespace, '-o', 'json', '--ignore-not-found').stdout
    return json.loads(response) if response else None


def wait(predicate, message, seconds=180):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        result = predicate()
        if result:
            return result
        time.sleep(0.5)
    raise AssertionError('Timeout: ' + message)


def ready(name, team='team-a', generation=None):
    def check():
        cr = get('database', name, team)
        status = cr.get('status', {}) if cr else {}
        if status.get('phase') == 'Failed':
            raise AssertionError(status['message'])
        return cr if status.get('phase') == 'Ready' and (
            generation is None or status.get('observedGeneration') == generation) else None
    return wait(check, f'{team}/{name} Ready')


def create(name, team='team-a'):
    kube('create', '-f', '-', obj={'apiVersion': 'platform.example.com/v1', 'kind': 'Database',
         'metadata': {'name': name, 'namespace': team, 'labels': {'test-run': RUN}},
         'spec': {'owner': team, 'extensions': ['pgcrypto'], 'connectionLimit': 20}})
    CREATED.append((name, team))


def credentials(name, team='team-a'):
    secret = get('secret', name + '-connection', team)
    assert secret and secret['metadata']['ownerReferences'][0]['kind'] == 'Database'
    return {k: base64.b64decode(v).decode() for k, v in secret['data'].items()}


def sql(query):
    return kube('exec', '-n', 'infra-backends', 'postgres-0', '--',
                'psql', '-U', 'postgres', '-d', 'postgres', '-v', 'ON_ERROR_STOP=1', '-Atc', query).stdout.strip()


def connect(name, team='team-a'):
    pod_name = name + '-connect'
    env = [{'name': pg, 'valueFrom': {'secretKeyRef': {'name': name + '-connection', 'key': key}}}
           for pg, key in [('PGHOST', 'host'), ('PGPORT', 'port'), ('PGDATABASE', 'database'),
                           ('PGUSER', 'username'), ('PGPASSWORD', 'password')]]
    kube('create', '-f', '-', obj={'apiVersion': 'v1', 'kind': 'Pod',
         'metadata': {'name': pod_name, 'namespace': team},
         'spec': {'restartPolicy': 'Never', 'automountServiceAccountToken': False,
                  'containers': [{'name': 'psql', 'image': 'postgres:17.11-alpine', 'env': env,
                                  'command': ['psql', '-v', 'ON_ERROR_STOP=1', '-Atc',
                                              'SELECT current_database(), current_user; SELECT count(*) FROM pg_extension WHERE extname=\'pgcrypto\';']}]}})
    try:
        wait(lambda: get('pod', pod_name, team)['status']['phase'] in ('Succeeded', 'Failed'), 'psql pod')
        assert get('pod', pod_name, team)['status']['phase'] == 'Succeeded'
        output = kube('logs', pod_name, '-n', team).stdout.strip()
        creds = credentials(name, team)
        assert output == f"{creds['database']}|{creds['username']}\n1", output
    finally:
        kube('delete', 'pod', pod_name, '-n', team, '--ignore-not-found', '--wait=false')


def delete_and_check(name, team, creds):
    kube('delete', 'databases.platform.example.com', name, '-n', team, '--wait=false')
    observed_deleting = False
    def gone():
        nonlocal observed_deleting
        cr = get('databases.platform.example.com', name, team)
        if cr:
            assert cr['metadata'].get('finalizers'), cr
            observed_deleting |= cr.get('status', {}).get('phase') == 'Deleting'
            return False
        # The CR is allowed to disappear only after external and backend cleanup.
        assert sql(f"SELECT count(*) FROM pg_database WHERE datname='{creds['database']}';") == '0'
        assert sql(f"SELECT count(*) FROM pg_roles WHERE rolname='{creds['username']}';") == '0'
        assert get('secret', f'tfstate-default-{team}-{name}', 'panko-state-' + team) is None
        assert get('secret', name + '-connection', team) is None
        return True
    wait(gone, f'{team}/{name} deleted after cleanup')
    assert observed_deleting, 'Deleting phase was never observed'


def main():
    name = RUN + '-orders'
    create(name)
    original = ready(name)
    creds = credentials(name)
    connect(name)
    print('1: CR Ready, owned connection secret, credential login and extension verified', flush=True)
    old_hash = original['status']['lastAppliedHash']
    changed = json.loads(kube('patch', 'databases.platform.example.com', name, '-n', 'team-a', '--type=merge',
                            '-p', json.dumps({'spec': {'connectionLimit': 31}}), '-o', 'json').stdout)
    updated = ready(name, generation=changed['metadata']['generation'])
    assert updated['status']['lastAppliedHash'] != old_hash
    jobs = json.loads(kube('get', 'jobs', '-n', 'team-a', '-l',
                         'platform.example.com/uid=' + original['metadata']['uid'], '-o', 'json').stdout)['items']
    assert any(j['metadata']['annotations']['platform.example.com/hash'] == updated['status']['lastAppliedHash']
               and any(c['type'] == 'Complete' and c['status'] == 'True' for c in j['status']['conditions']) for j in jobs)
    current_job = next(j for j in jobs if j['metadata']['annotations']['platform.example.com/hash'] == updated['status']['lastAppliedHash'])
    logs = kube('logs', '-n', 'team-a', 'job/' + current_job['metadata']['name']).stdout
    events = [json.loads(line) for line in logs.splitlines() if line.startswith('{"event":')]
    succeeded = next(event for event in events if event['event'] == 'execution_succeeded')
    assert succeeded['uid'] == original['metadata']['uid']
    assert succeeded['generation'] == changed['metadata']['generation']
    assert succeeded['job'] == current_job['metadata']['name']
    assert not ({'spec', 'password', 'token'} & succeeded.keys())
    assert sql(f"SELECT datconnlimit FROM pg_database WHERE datname='{creds['database']}';") == '31'
    assert sql(f"SELECT rolconnlimit FROM pg_roles WHERE rolname='{creds['username']}';") == '31'
    print('2: new generation completed in a new job; PostgreSQL database and role limits changed', flush=True)
    for patch, marker in [({'connectionLimit': 0}, 'connectionLimit'), ({'owner': 'other'}, 'immutable')]:
        response = kube('patch', 'databases.platform.example.com', name, '-n', 'team-a', '--type=merge', '-p',
                        json.dumps({'spec': patch}), check=False)
        assert response.returncode and marker in response.stderr, response
    print('3: schema and CEL violations rejected by API server', flush=True)
    delete_and_check(name, 'team-a', creds)
    print('4: CR stayed Deleting until database, role, connection secret and state were removed', flush=True)
    parallel = [(RUN + '-parallel-a', 'team-a'), (RUN + '-parallel-b', 'team-b')]
    for other, team in parallel:
        create(other, team)
    for other, team in parallel:
        ready(other, team)
        connect(other, team)
    print('5: parallel CRs in two team namespaces independently Ready and usable', flush=True)
    for other, team in parallel:
        delete_and_check(other, team, credentials(other, team))
    prefix = (RUN + '-same-prefix-').ljust(40, 'x')[:40]
    colliding = [prefix + '-a', prefix + '-b']
    for other in colliding:
        create(other)
    job_names = []
    for other in colliding:
        cr = ready(other)
        jobs = json.loads(kube('get', 'jobs', '-n', 'team-a', '-l',
                             'platform.example.com/uid=' + cr['metadata']['uid'], '-o', 'json').stdout)['items']
        assert jobs, other
        job_names.append(jobs[0]['metadata']['name'])
    assert len(set(job_names)) == 2, job_names
    for other in colliding:
        delete_and_check(other, 'team-a', credentials(other))
    print('6: resources with identical 40-character prefixes provisioned independently', flush=True)
    print('All Kubernetes/PostgreSQL end-to-end tests passed', flush=True)


if __name__ == '__main__':
    try:
        main()
    finally:
        # Keep failed Jobs/logs for diagnosis; request normal finalizer cleanup for CRs.
        for resource, namespace in CREATED:
            kube('delete', 'databases.platform.example.com', resource, '-n', namespace, '--ignore-not-found', '--wait=false', check=False)
