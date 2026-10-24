const assert = require('node:assert/strict');
const { test } = require('node:test');
const gate = require('./release-gate.cjs');

const sha = 'a'.repeat(40);
const goodRun = { id: 42, run_number: 8, head_sha: sha, head_branch: 'main', event: 'push', status: 'completed', conclusion: 'success', html_url: 'https://example.com/run/42' };
const goodJobs = ['unit', 'e2e'].map(name => ({ name, status: 'completed', conclusion: 'success' }));

function fixture({ runs = [goodRun], jobs = goodJobs, ref = 'refs/tags/v0.0.1' } = {}) {
  const outputs = {};
  const requests = [];
  const github = {
    rest: { actions: { listWorkflowRuns: 'runs', listJobsForWorkflowRun: 'jobs' } },
    paginate: async (method, args) => {
      requests.push({ method, args });
      return method === 'runs' ? runs : jobs;
    },
  };
  return { outputs, requests, args: { github, sha, context: { ref, repo: { owner: 'hingst-it', repo: 'panko' } }, core: { info() {}, setOutput: (key, value) => { outputs[key] = value; } } } };
}

test('accepts the exact main commit with both required jobs', async () => {
  const f = fixture();
  await gate(f.args);
  assert.deepEqual(f.outputs, { tag: 'v0.0.1', sha, prerelease: false });
  assert.equal(f.requests[0].args.head_sha, sha);
  assert.equal(f.requests[0].args.workflow_id, 'ci.yaml');
  assert.equal(f.requests[1].args.filter, 'latest');
});

test('identifies prereleases', async () => {
  const f = fixture({ ref: 'refs/tags/v0.0.1-rc.1' });
  await gate(f.args);
  assert.equal(f.outputs.prerelease, true);
});

for (const ref of ['refs/heads/main', 'refs/tags/v1', 'refs/tags/v01.0.0', 'refs/tags/v1.0.0-rc.01', 'refs/tags/v1.0.0+build']) {
  test(`rejects unsupported ref ${ref} before querying CI`, async () => {
    const f = fixture({ ref });
    await assert.rejects(gate(f.args), /Release tags/);
    assert.equal(f.requests.length, 0);
  });
}

for (const changes of [{ head_sha: 'b'.repeat(40) }, { head_branch: 'feature' }, { event: 'pull_request' }, { status: 'in_progress' }, { conclusion: 'failure' }, { conclusion: 'cancelled' }]) {
  test(`rejects CI with ${JSON.stringify(changes)}`, async () => {
    await assert.rejects(gate(fixture({ runs: [{ ...goodRun, ...changes }] }).args), /successful Checks run/);
  });
}

test('rejects missing CI and a newer failed run despite an older success', async () => {
  await assert.rejects(gate(fixture({ runs: [] }).args), /successful Checks run/);
  await assert.rejects(gate(fixture({ runs: [goodRun, { ...goodRun, run_number: 9, conclusion: 'failure' }] }).args), /successful Checks run/);
});

for (const jobs of [[], [goodJobs[0]], [goodJobs[1]], [goodJobs[0], { ...goodJobs[1], conclusion: 'skipped' }]]) {
  test(`rejects missing or skipped required jobs: ${JSON.stringify(jobs)}`, async () => {
    await assert.rejects(gate(fixture({ jobs }).args), /must include a successful/);
  });
}
