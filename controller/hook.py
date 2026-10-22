"""Metacontroller hook: schedule immutable jobs and report observed state."""
from copy import deepcopy
from datetime import datetime, timezone
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os

PREFIX = 'platform.example.com/'


def digest(parent):
    payload = {'spec': parent['spec'], 'generation': parent['metadata']['generation']}
    return hashlib.sha256(json.dumps(payload, sort_keys=True, separators=(',', ':')).encode()).hexdigest()


def terminal(job):
    for condition in job.get('status', {}).get('conditions', []):
        if condition['status'] == 'True' and condition['type'] in ('Complete', 'Failed'):
            return condition['type']
    return None


def annotations(job):
    return job['metadata']['annotations']


def job(parent, action, hash_value, generation):
    meta = parent['metadata']
    name = f"db-{meta['name'][:40]}-{hash_value[:8]}"
    if action == 'destroy':
        name += '-destroy'
    return {
        'apiVersion': 'batch/v1', 'kind': 'Job',
        'metadata': {'name': name, 'namespace': meta['namespace'],
                     'labels': {PREFIX + 'uid': meta['uid']},
                     'annotations': {PREFIX + 'hash': hash_value, PREFIX + 'action': action,
                                     PREFIX + 'generation': str(generation)}},
        'spec': {'backoffLimit': 0, 'activeDeadlineSeconds': 300, 'ttlSecondsAfterFinished': 3600,
                 'template': {'spec': {
                     'serviceAccountName': 'tofu-runner', 'restartPolicy': 'Never',
                     'securityContext': {'runAsNonRoot': True, 'runAsUser': 1000, 'fsGroup': 1000},
                     'containers': [{'name': 'tofu', 'image': os.getenv('RUNNER_IMAGE', 'panko-runner:dev'),
                                     'imagePullPolicy': 'Never',
                                     'args': [action, meta['namespace'], meta['name']],
                                     'securityContext': {'allowPrivilegeEscalation': False,
                                                         'readOnlyRootFilesystem': True,
                                                         'capabilities': {'drop': ['ALL']}},
                                     'resources': {'requests': {'cpu': '100m', 'memory': '128Mi'},
                                                   'limits': {'memory': '512Mi'}},
                                     'volumeMounts': [{'name': 'work', 'mountPath': '/work'},
                                                      {'name': 'tmp', 'mountPath': '/tmp'}]}],
                     'volumes': [{'name': 'work', 'emptyDir': {}}, {'name': 'tmp', 'emptyDir': {}}],
                 }}}}


def retain(parent, child):
    ann = annotations(child)
    return job(parent, ann[PREFIX + 'action'], ann[PREFIX + 'hash'], int(ann[PREFIX + 'generation']))


def result(parent, phase, generation, message, children, applied=None, finalized=None):
    status = deepcopy(parent.get('status', {}))
    status.update(phase=phase, observedGeneration=generation, message=message)
    if applied:
        status['lastAppliedHash'] = applied
        status['connectionSecretRef'] = {'name': parent['metadata']['name'] + '-connection'}
    condition = {'type': 'Ready', 'status': 'True' if phase == 'Ready' else 'False',
                 'reason': phase, 'message': message, 'observedGeneration': generation,
                 'lastTransitionTime': datetime.now(timezone.utc).isoformat().replace('+00:00', 'Z')}
    previous = next((c for c in status.get('conditions', []) if c['type'] == 'Ready'), {})
    if all(condition[k] == previous.get(k) for k in ('status', 'reason')):
        condition['lastTransitionTime'] = previous['lastTransitionTime']
    status['conditions'] = [condition]
    response = {'status': status, 'children': children, 'resyncAfterSeconds': 5}
    if finalized is not None:
        response['finalized'] = finalized
    return response


def reconcile(request):
    parent = request['parent']
    meta = parent['metadata']
    generation, hash_value = meta['generation'], digest(parent)
    children = list(request.get('children', {}).get('Job.batch/v1', {}).values())
    running = [c for c in children if not terminal(c)]
    finalizing = request.get('finalizing', False)
    if finalizing:
        if running:
            return result(parent, 'Deleting', generation, 'Waiting for running job before teardown',
                          [retain(parent, c) for c in running], finalized=False)
        destroy = next((c for c in children if annotations(c).get(PREFIX + 'action') == 'destroy'), None)
        if destroy:
            success = terminal(destroy) == 'Complete'
            return result(parent, 'Deleting', generation,
                          'Resources and state removed' if success else 'Destroy failed; inspect job logs',
                          [] if success else [retain(parent, destroy)], finalized=success)
        return result(parent, 'Deleting', generation, 'Destroy scheduled',
                      [job(parent, 'destroy', hash_value, generation)], finalized=False)
    if running:
        child = running[0]
        return result(parent, 'Applying', int(annotations(child)[PREFIX + 'generation']),
                      'OpenTofu apply is running', [retain(parent, c) for c in running])
    current = next((c for c in children if annotations(c).get(PREFIX + 'hash') == hash_value
                    and annotations(c).get(PREFIX + 'action') == 'apply'), None)
    if current:
        if terminal(current) == 'Complete':
            return result(parent, 'Ready', generation, 'Database is ready',
                          [retain(parent, current)], applied=hash_value)
        return result(parent, 'Failed', generation, 'Apply failed; inspect job logs and update spec to retry',
                      [retain(parent, current)])
    old = parent.get('status', {})
    if old.get('lastAppliedHash') == hash_value:
        return result(parent, 'Ready', generation, 'Database is ready', [], applied=hash_value)
    if old.get('phase') == 'Failed' and old.get('observedGeneration') == generation:
        return result(parent, 'Failed', generation, old['message'], [])
    return result(parent, 'Pending', generation, 'Apply scheduled',
                  [job(parent, 'apply', hash_value, generation)])


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200 if self.path == '/healthz' else 404)
        self.end_headers()

    def do_POST(self):
        try:
            request = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
            body = json.dumps(reconcile(request)).encode()
            self.send_response(200)
        except Exception:
            import traceback
            traceback.print_exc()
            self.send_response(500)
            body = b'{"error":"hook failed"}'
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)


if __name__ == '__main__':
    ThreadingHTTPServer(('0.0.0.0', 8080), Handler).serve_forever()
