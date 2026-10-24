// Called by actions/github-script. Keep the release authorization read-only.
module.exports = async ({ github, context, core, sha }) => {
  const tag = context.ref.replace(/^refs\/tags\//, '');
  const version = /^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$/.exec(tag);
  if (!context.ref.startsWith('refs/tags/') || !version ||
      (version[4] && version[4].split('.').some(part => /^0\d+$/.test(part)))) {
    throw new Error('Release tags must use vMAJOR.MINOR.PATCH, optionally with a SemVer prerelease suffix.');
  }

  const runs = await github.paginate(github.rest.actions.listWorkflowRuns, {
    ...context.repo, workflow_id: 'ci.yaml', branch: 'main', event: 'push',
    head_sha: sha, per_page: 100,
  });
  // Do not let an older successful run hide a failed or unfinished newer run.
  const run = runs.filter(run => run.head_sha === sha && run.head_branch === 'main' && run.event === 'push')
    .sort((a, b) => b.run_number - a.run_number)[0];
  if (!run || run.status !== 'completed' || run.conclusion !== 'success') {
    throw new Error(`The tagged commit ${sha} needs a completed, successful Checks run from a push to main. Wait for CI, then re-run this release workflow.`);
  }
  const jobs = await github.paginate(github.rest.actions.listJobsForWorkflowRun, {
    ...context.repo, run_id: run.id, filter: 'latest', per_page: 100,
  });
  for (const name of ['unit', 'e2e']) {
    const job = jobs.find(job => job.name === name);
    if (!job || job.status !== 'completed' || job.conclusion !== 'success') {
      throw new Error(`Checks run ${run.id} must include a successful ${name} job; skipped jobs do not qualify.`);
    }
  }
  core.info(`Release ${tag} authorized by ${run.html_url}`);
  core.setOutput('tag', tag);
  core.setOutput('sha', sha);
  core.setOutput('prerelease', Boolean(version[4]));
};
