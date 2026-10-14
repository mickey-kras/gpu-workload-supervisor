const { verifyPublished } = require('./release-publication.cjs');

const MAX_AGE_MS = 24 * 60 * 60 * 1000;
const WAIT_MS = 70 * 60 * 1000;
const POLL_MS = 30000;
const REQUIRED_JOBS = new Map([
  ['quality / checks', ['Go vulnerability audit', 'Audit Aislop toolchain',
    'Test policy automation', 'Gitleaks', 'Semgrep', 'Trivy high and critical gate']],
  ['aislop / aislop status', ['Aislop automation quality gate', 'Aislop Go quality gate', 'Aislop findings gate']],
  ['codeql / analyze', ['Run github/codeql-action/analyze@']],
  ['sonar', ['SonarQube analysis', 'SonarQube quality gate']],
]);

async function verifyCandidate({ github, context, core, version }) {
  if (context.eventName !== 'workflow_dispatch' || context.ref !== 'refs/heads/main' ||
      !/^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/.test(version) ||
      !/^[a-f0-9]{40}$/.test(context.sha)) {
    throw new Error('Release requires dispatch from main, an exact SHA and a plain SemVer version');
  }
  const { data: tip } = await github.rest.repos.getBranch({ ...context.repo, branch: 'main' });
  if (tip.commit.sha === context.sha) return;
  try {
    const { data: candidate } = await github.rest.git.getRef({
      ...context.repo, ref: `heads/release/${version}`,
    });
    if (candidate.object.type !== 'commit' || candidate.object.sha !== context.sha) {
      throw new Error('Main moved and the prepared branch has another SHA');
    }
    core.info('Retrying the prepared commit after main advanced');
  } catch (error) {
    if (error.status !== 404) throw error;
    await verifyPublished({ github, context, version });
    core.info('Retrying the published commit after branch cleanup');
  }
}

function trustedRun(run, context, repository, workflow) {
  if (!Number.isInteger(run.id) || !Number.isInteger(run.run_number) ||
      !Number.isInteger(run.run_attempt) || run.run_attempt < 1 ||
      run.workflow_id !== workflow.id ||
      ![workflow.path, `${workflow.path}@main`].includes(run.path) ||
      run.repository?.id !== repository.id || run.repository?.full_name !== repository.full_name ||
      run.head_repository?.id !== repository.id ||
      run.head_repository?.full_name !== repository.full_name ||
      run.head_branch !== 'main' || run.head_sha !== context.sha ||
      !['push', 'workflow_dispatch'].includes(run.event)) {
    throw new Error('Main validation has mismatched workflow, repository, event, branch, SHA or attempt');
  }
}

async function latestMainRun(github, context, repository, workflow) {
  const runs = await github.paginate(github.rest.actions.listWorkflowRuns, {
    ...context.repo, workflow_id: workflow.id, branch: 'main', head_sha: context.sha, per_page: 100,
  });
  for (const run of runs) trustedRun(run, context, repository, workflow);
  return runs.toSorted((a, b) => b.run_number - a.run_number)[0];
}

function timestamp(value) {
  const result = Date.parse(value);
  if (typeof value !== 'string' || !Number.isFinite(result)) {
    throw new Error('Main validation has missing or invalid timestamps');
  }
  return result;
}

function verifyJobs(jobs, run, now) {
  const required = new Map(REQUIRED_JOBS);
  if (jobs.some(job => job.name === 'quality / scans')) {
    required.set('quality / checks', []);
    required.set('quality / go', ['Go vulnerability audit']);
    required.set('quality / frontend', ['Audit Aislop toolchain', 'Test policy automation']);
    required.set('quality / package', []);
    required.set('quality / scans', ['Gitleaks', 'Semgrep', 'Trivy high and critical gate']);
    for (let shard = 0; shard < 6; shard++) required.set(`quality / native (${shard})`, []);
  }
  for (const [name, steps] of required) {
    const matching = jobs.filter(job => job.name === name);
    const job = matching[0];
    if (matching.length !== 1 || job.run_id !== run.id || job.run_attempt !== run.run_attempt ||
        job.head_sha !== run.head_sha || job.status !== 'completed' || job.conclusion !== 'success') {
      throw new Error(`Main validation lacks a successful exact-attempt job: ${name}`);
    }
    const started = timestamp(job.started_at);
    const completed = timestamp(job.completed_at);
    if (started > completed || completed > now || now - started > MAX_AGE_MS) {
      throw new Error(`Main validation is stale or has invalid job times: ${name}`);
    }
    for (const expected of steps) {
      const found = job.steps?.filter(step => expected.endsWith('@')
        ? step.name?.startsWith(expected) : step.name === expected) || [];
      if (found.length !== 1 || found[0].status !== 'completed' || found[0].conclusion !== 'success') {
        throw new Error(`Main validation lacks a successful step: ${expected}`);
      }
    }
  }
}

async function verifyMain({ github, context, core, version,
  sleep = ms => new Promise(resolve => setTimeout(resolve, ms)), now = Date.now }) {
  await verifyCandidate({ github, context, core, version });
  const { data: repository } = await github.rest.repos.get(context.repo);
  const { data: workflow } = await github.rest.actions.getWorkflow({ ...context.repo, workflow_id: 'main.yml' });
  if (!Number.isInteger(repository.id) || repository.full_name !== `${context.repo.owner}/${context.repo.repo}` ||
      !Number.isInteger(workflow.id) || workflow.path !== '.github/workflows/main.yml' || workflow.state !== 'active') {
    throw new Error('Cannot establish the trusted main workflow and repository');
  }
  const deadline = now() + WAIT_MS;
  for (;;) {
    const latest = await latestMainRun(github, context, repository, workflow);
    if (latest) {
      const { data: run } = await github.rest.actions.getWorkflowRun({ ...context.repo, run_id: latest.id });
      trustedRun(run, context, repository, workflow);
      if (run.id !== latest.id) throw new Error('Main validation run identity changed');
      if (run.status === 'completed') {
        if (run.conclusion !== 'success') throw new Error('Main validation failed for the release commit');
        const jobs = await github.paginate(github.rest.actions.listJobsForWorkflowRunAttempt, {
          ...context.repo, run_id: run.id, attempt_number: run.run_attempt, per_page: 100,
        });
        verifyJobs(jobs, run, now());
        const { data: current } = await github.rest.actions.getWorkflowRun({ ...context.repo, run_id: run.id });
        trustedRun(current, context, repository, workflow);
        if (current.id !== run.id || current.run_attempt !== run.run_attempt ||
            current.status !== 'completed' || current.conclusion !== 'success') {
          throw new Error('Main validation changed while checking its evidence');
        }
        await verifyCandidate({ github, context, core, version });
        const authorized = await latestMainRun(github, context, repository, workflow);
        if (authorized?.id !== run.id || authorized.run_attempt !== run.run_attempt ||
            authorized.status !== 'completed' || authorized.conclusion !== 'success') {
          throw new Error('Main validation was superseded while checking its evidence');
        }
        verifyJobs(jobs, run, now());
        core.info(`Reusing main run ${run.id} attempt ${run.run_attempt} for ${context.sha}`);
        return { run_id: run.id, run_attempt: run.run_attempt };
      }
    }
    if (now() >= deadline) throw new Error('Timed out waiting for successful main validation');
    core.info('Waiting for main validation at the release commit');
    await sleep(POLL_MS);
  }
}

module.exports = { verifyCandidate, verifyMain };
