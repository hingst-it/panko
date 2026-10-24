"""Backup and restore rehearsal: run `prepare` before the backup and `verify` after restoring into a fresh lab."""
import hashlib
import json
import os
from pathlib import Path
import sys

from e2e import credentials, delete_and_check, get, kube, ready, wait

EXPECT = Path('.local/restore-expect.json')
S3 = os.environ.get('PANKO_RESTORE_S3', 'true') == 'true'
NAMES = ['keep-k8s'] + (['keep-s3'] if S3 else [])


def database(name, ref=None):
    spec = {'owner': 'team-a', 'extensions': [], 'connectionLimit': 20}
    if ref:
        spec['stateBackendRef'] = {'name': ref}
    kube('create', '-f', '-', obj={'apiVersion': 'platform.example.com/v1', 'kind': 'Database',
         'metadata': {'name': name, 'namespace': 'team-a'}, 'spec': spec})


def psql(database, query):
    return kube('exec', '-n', 'infra-backends', 'postgres-0', '--', 'psql', '-U', 'postgres', '-d', database,
                '-v', 'ON_ERROR_STOP=1', '-Atc', query).stdout.strip()


def fingerprint(creds):
    return hashlib.sha256(json.dumps(creds, sort_keys=True).encode()).hexdigest()


def prepare():
    if S3:
        kube('apply', '-f', '-', obj={'apiVersion': 'platform.example.com/v1', 'kind': 'StateBackend',
             'metadata': {'name': 'restore-s3', 'namespace': 'team-a'},
             'spec': {'profileRef': {'name': 'lab-s3'}, 'credentialsSecretRef': {'name': 'panko-state-s3'}}})
    database('keep-k8s')
    if S3:
        database('keep-s3', 'restore-s3')
    expected = {}
    for name in NAMES:
        ready(name)
        creds = credentials(name)
        psql(creds['database'], "CREATE TABLE keep (value text); INSERT INTO keep VALUES ('survives restore');")
        expected[name] = {'uid': get('db', name)['metadata']['uid'], 'database': creds['database'],
                          'username': creds['username'], 'credentials': fingerprint(creds),
                          'backend': get('db', name)['status']['stateBackend']['type']}
    EXPECT.write_text(json.dumps(expected))
    EXPECT.chmod(0o600)
    print('Prepared', ', '.join(NAMES), 'with data rows', flush=True)


def verify():
    expected = json.loads(EXPECT.read_text())
    for name, old in expected.items():
        db = ready(name)
        status = db['status']
        assert db['metadata']['uid'] != old['uid'] and status['identity'] == old['uid'], (db['metadata']['uid'], status.get('identity'))
        assert status['stateBackend']['type'] == old['backend'], status['stateBackend']
        creds = credentials(name)
        assert (creds['database'], creds['username']) == (old['database'], old['username']), 'database or role renamed'
        assert fingerprint(creds) == old['credentials'], 'credentials changed by the restore'
        assert psql(creds['database'], 'SELECT value FROM keep;') == 'survives restore', 'data lost'
        generation = db['metadata']['generation'] + 1
        kube('patch', 'db', name, '-n', 'team-a', '--type=merge', '-p', '{"spec":{"connectionLimit":25}}')
        ready(name, generation=generation)
        assert credentials(name)['database'] == old['database'], 'update after restore recreated the database'
        assert psql(old['database'], 'SELECT value FROM keep;') == 'survives restore'
        print(f'{name}: restored with identity, credentials and data; update kept the database', flush=True)
    for name, old in expected.items():
        creds = credentials(name)
        delete_and_check(name, 'team-a', creds)
    kube('delete', 'statebackend', 'restore-s3', '-n', 'team-a', '--ignore-not-found', check=False)
    wait(lambda: all(get('db', name) is None for name in expected), 'restored resources deleted', 300)
    print('Restore rehearsal passed: data, identity and credentials survived; update and destroy worked', flush=True)


if __name__ == '__main__':
    {'prepare': prepare, 'verify': verify}[sys.argv[1]]()
