import copy
from pathlib import Path
import sys
import unittest
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'controller'))
import hook


class HookTest(unittest.TestCase):
    def setUp(self):
        self.parent = {'metadata': {'name': 'orders', 'namespace': 'team-a', 'uid': 'abc', 'generation': 1},
                       'spec': {'owner': 'team-a', 'extensions': [], 'connectionLimit': 20}}

    def request(self, *children, finalizing=False):
        return {'parent': self.parent, 'children': {'Job.batch/v1': {c['metadata']['name']: c for c in children}},
                'finalizing': finalizing}

    def test_changed_spec_waits_for_active_job(self):
        child = hook.reconcile(self.request())['children'][0]
        self.parent['spec']['connectionLimit'] = 30
        self.parent['metadata']['generation'] = 2
        response = hook.reconcile(self.request(child))
        self.assertEqual(response['children'][0]['metadata']['name'], child['metadata']['name'])
        self.assertEqual(response['status']['observedGeneration'], 1)
        child['status'] = {'conditions': [{'type': 'Complete', 'status': 'True'}]}
        response = hook.reconcile(self.request(child))
        self.assertNotEqual(response['children'][0]['metadata']['name'], child['metadata']['name'])

    def test_ready_survives_ttl(self):
        child = hook.reconcile(self.request())['children'][0]
        child['status'] = {'conditions': [{'type': 'Complete', 'status': 'True'}]}
        self.parent['status'] = hook.reconcile(self.request(child))['status']
        response = hook.reconcile(self.request())
        self.assertEqual(response['status']['phase'], 'Ready')
        self.assertEqual(response['children'], [])

    def test_finalize_waits_for_apply_then_destroy_success(self):
        child = hook.reconcile(self.request())['children'][0]
        response = hook.reconcile(self.request(child, finalizing=True))
        self.assertFalse(response['finalized'])
        self.assertEqual(response['status']['phase'], 'Deleting')
        child['status'] = {'conditions': [{'type': 'Complete', 'status': 'True'}]}
        destroy = hook.reconcile(self.request(child, finalizing=True))['children'][0]
        failed = copy.deepcopy(destroy)
        failed['status'] = {'conditions': [{'type': 'Failed', 'status': 'True'}]}
        self.assertFalse(hook.reconcile(self.request(failed, finalizing=True))['finalized'])
        destroy['status'] = {'conditions': [{'type': 'Complete', 'status': 'True'}]}
        self.assertTrue(hook.reconcile(self.request(destroy, finalizing=True))['finalized'])


if __name__ == '__main__':
    unittest.main()
