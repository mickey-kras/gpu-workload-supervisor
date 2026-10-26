const test = require('node:test');
const assert = require('node:assert/strict');
const { verifyCandidate, verifyMain } = require('./release-validation.cjs');
const { VALIDATION_SCRIPT } = require('./policy-release.cjs');
const { mkdtempSync, mkdirSync, readFileSync, writeFileSync, symlinkSync, rmSync } = require('node:fs');
const { tmpdir } = require('node:os');
const { join, resolve } = require('node:path');
const { execFileSync } = require('node:child_process');

const SHA = 'a'.repeat(40);
const OTHER = 'b'.repeat(40);
const VERSION = '0.1.0';
const FULL_NAME = 'mickey-kras/gpu-workload-supervisor';
const NOW = Date.parse('2026-10-14T12:00:00Z');
const time = delta => new Date(NOW + delta).toISOString();

function splitQuality(input) {
  const original = input.state.jobs[0];
  original.steps = [];
  for (const [name, steps] of [
    ['quality / go', ['Go vulnerability audit']],
    ['quality / frontend', ['Audit Aislop toolchain', 'Test policy automation']],
    ['quality / scans', ['Gitleaks', 'Semgrep', 'Trivy high and critical gate']],
    ['quality / package', []],
    ['quality / docs-ste / ste-cli', []],
    ['quality / docs-ste / vale', []],
    ...Array.from({ length: 6 }, (_, shard) => [`quality / native (${shard})`, []]),
  ]) {
    input.state.jobs.push({ ...structuredClone(original), id: input.state.jobs.length + 1, name,
      steps: steps.map(name => ({ name, status: 'completed', conclusion: 'success' })) });
  }
}

function fixture() {
  const repository = { id: 1394331134, full_name: FULL_NAME };
  const workflow = { id: 123, path: '.github/workflows/main.yml', state: 'active' };
  const run = { id: 456, run_number: 7, run_attempt: 1, workflow_id: workflow.id,
    path: workflow.path, repository, head_repository: repository, head_branch: 'main',
    head_sha: SHA, event: 'push', status: 'completed', conclusion: 'success' };
  const names = [
    ['quality / checks', ['Go vulnerability audit', 'Audit Aislop toolchain',
      'Test policy automation', 'Gitleaks', 'Semgrep', 'Trivy high and critical gate']],
    ['aislop / aislop status', ['Aislop automation quality gate', 'Aislop Go quality gate', 'Aislop findings gate']],
    ['codeql / analyze', ['Run github/codeql-action/analyze@' + 'c'.repeat(40)]],
    ['sonar', ['SonarQube analysis', 'SonarQube quality gate']],
  ];
  const jobs = names.map(([name, steps], index) => ({ id: index + 1, name,
    run_id: run.id, run_attempt: run.run_attempt, head_sha: SHA,
    status: 'completed', conclusion: 'success', started_at: time(-3600000), completed_at: time(-60000),
    steps: steps.map((name, index) => ({ name, number: index + 1, status: 'completed', conclusion: 'success' })) }));
  const state = { repository, workflow, run, jobs, clock: NOW, main: SHA, frozen: null,
    runs: [run], reads: [], queries: [], sleeps: [], logs: [], release: null };
  const github = { rest: {
    repos: {
      get: async () => ({ data: state.repository }),
      getBranch: async () => ({ data: { commit: { sha: state.main } } }),
      getReleaseByTag: async () => ({ data: state.release }),
    },
    git: { getRef: async ({ ref }) => {
      if (ref.startsWith('tags/')) return { data: { object: { type: 'commit', sha: SHA } } };
      if (!state.frozen) throw Object.assign(new Error('Not found'), { status: 404 });
      return { data: { object: { type: 'commit', sha: state.frozen } } };
    } },
    actions: {
      getWorkflow: async params => {
        assert.equal(params.workflow_id, 'main.yml'); return { data: state.workflow };
      },
      listWorkflowRuns() {}, listJobsForWorkflowRunAttempt() {},
      getWorkflowRun: async params => {
        state.reads.push(params); return { data: structuredClone(state.run) };
      },
    },
  }, paginate: async (method, params) => {
    state.queries.push(params);
    if (method === github.rest.actions.listWorkflowRuns) return structuredClone(state.runs);
    assert.equal(method, github.rest.actions.listJobsForWorkflowRunAttempt);
    assert.equal(params.run_id, state.run.id);
    assert.equal(params.attempt_number, state.run.run_attempt);
    return structuredClone(state.jobs);
  } };
  return { state, github, version: VERSION, context: { eventName: 'workflow_dispatch',
    ref: 'refs/heads/main', sha: SHA, repo: { owner: 'mickey-kras', repo: 'gpu-workload-supervisor' } },
  core: { info: value => state.logs.push(value) }, now: () => state.clock,
  sleep: async ms => { state.sleeps.push(ms); state.clock += ms; } };
}

test('reuses successful exact-SHA main evidence including scanner and Sonar steps', async () => {
  const input = fixture();
  assert.deepEqual(await verifyMain(input), { run_id: 456, run_attempt: 1 });
  assert.equal(input.state.queries[0].head_sha, SHA);
  assert.equal(input.state.queries[0].branch, 'main');
  assert.equal(input.state.queries[0].workflow_id, 123);
  assert.equal(input.state.queries[1].attempt_number, 1);
  assert.deepEqual(input.state.sleeps, []);
});

test('dispatch while main is running waits beyond the old ten-minute window', async () => {
  const input = fixture(); input.state.run.status = 'in_progress'; input.state.run.conclusion = null;
  input.sleep = async ms => {
    input.state.clock += ms; input.state.sleeps.push(ms);
    if (input.state.clock - NOW >= 40 * 60000) {
      input.state.run.status = 'completed'; input.state.run.conclusion = 'success';
    }
  };
  await verifyMain(input);
  assert.equal(input.state.sleeps.length, 80);
});

test('missing validation and queued main runs time out after bounded waiting', async () => {
  for (const pending of ['missing', 'queued']) {
    const input = fixture();
    if (pending === 'missing') input.state.runs = [];
    else { input.state.run.status = 'queued'; input.state.run.conclusion = null; }
    await assert.rejects(verifyMain(input), /Timed out/);
    assert.equal(input.state.clock - NOW, 70 * 60000);
  }
});

test('failed, cancelled, skipped or neutral main validation cannot publish', async () => {
  for (const conclusion of ['failure', 'cancelled', 'skipped', 'neutral', 'timed_out', null]) {
    const input = fixture(); input.state.run.conclusion = conclusion;
    await assert.rejects(verifyMain(input), /Main validation failed/);
  }
});

test('a newer failing run supersedes an older success', async () => {
  const input = fixture();
  input.state.runs.unshift({ ...input.state.run, id: 789, run_number: 8, conclusion: 'failure' });
  input.state.run = input.state.runs[0];
  await assert.rejects(verifyMain(input), /Main validation failed/);
  assert.equal(input.state.reads[0].run_id, 789);
});

test('a new pending or failing run discovered during job inspection supersedes the validated run', async () => {
  for (const status of ['in_progress', 'completed']) {
    const input = fixture(); const paginate = input.github.paginate;
    input.github.paginate = async (method, params) => {
      const result = await paginate(method, params);
      if (method === input.github.rest.actions.listJobsForWorkflowRunAttempt) {
        input.state.runs.push({ ...structuredClone(input.state.run), id: 789, run_number: 8,
          status, conclusion: status === 'completed' ? 'failure' : null });
      }
      return result;
    };
    await assert.rejects(verifyMain(input), /superseded/);
  }
});

test('workflow run paths support only the exact workflow path or its main qualification', async () => {
  const allowed = fixture(); allowed.state.run.path += '@main';
  await verifyMain(allowed);
  for (const path of ['.github/workflows/main.yml@other', '.github/workflows/main.yml@refs/heads/main',
    '.github/workflows/main.yml@main-extra', '.github/workflows/main.yaml@main']) {
    const input = fixture(); input.state.run.path = path;
    await assert.rejects(verifyMain(input), /mismatched/);
  }
});

test('workflow, repository, origin, event, branch, SHA and attempt mismatches fail closed', async () => {
  for (const mutate of [
    run => { run.workflow_id++; }, run => { run.path = '.github/workflows/pr-validation.yml'; },
    run => { run.repository = { id: 1, full_name: FULL_NAME }; },
    run => { run.head_repository = { id: 1, full_name: 'attacker/fork' }; },
    run => { run.event = 'pull_request'; }, run => { run.head_branch = 'release/0.1.0'; },
    run => { run.head_sha = OTHER; }, run => { run.run_attempt = 0; },
    run => { delete run.run_attempt; },
  ]) {
    const input = fixture(); mutate(input.state.run);
    await assert.rejects(verifyMain(input), /mismatched/);
  }
});

test('missing or disabled trusted workflow and wrong repository identity are rejected', async () => {
  for (const mutate of [
    state => { state.workflow.state = 'disabled_manually'; },
    state => { state.workflow.path = '.github/workflows/other.yml'; },
    state => { state.repository.full_name = 'other/repo'; },
    state => { delete state.repository.id; },
  ]) {
    const input = fixture(); mutate(input.state);
    await assert.rejects(verifyMain(input), /Cannot establish/);
  }
});

test('each required job must succeed at the exact SHA, run and attempt', async () => {
  for (const mutate of [
    jobs => { jobs.pop(); }, jobs => { jobs.push(structuredClone(jobs[0])); },
    jobs => { jobs[0].conclusion = 'failure'; }, jobs => { jobs[0].status = 'in_progress'; },
    jobs => { jobs[0].run_id++; }, jobs => { jobs[0].run_attempt++; },
    jobs => { jobs[0].head_sha = OTHER; },
  ]) {
    const input = fixture(); mutate(input.state.jobs);
    await assert.rejects(verifyMain(input), /exact-attempt job/);
  }
});

test('scanner and Sonar steps cannot be missing or skipped inside successful jobs', async () => {
  for (let index = 0; index < 4; index++) {
    for (const mutation of ['missing', 'skipped', 'duplicate']) {
      const input = fixture(); const steps = input.state.jobs[index].steps;
      if (mutation === 'missing') steps.pop();
      if (mutation === 'skipped') steps[steps.length - 1].conclusion = 'skipped';
      if (mutation === 'duplicate') steps.push(structuredClone(steps[0]));
      await assert.rejects(verifyMain(input), /successful step/);
    }
  }
});

test('freshness starts before scans, and invalid or future job timestamps are rejected', async () => {
  for (const mutate of [
    job => { job.started_at = time(-24 * 3600000 - 1); },
    job => { job.started_at = 'invalid'; }, job => { delete job.completed_at; },
    job => { job.completed_at = time(1); }, job => { job.started_at = time(-1); },
  ]) {
    const input = fixture(); mutate(input.state.jobs[0]);
    await assert.rejects(verifyMain(input), /stale|timestamps|job times/);
  }
  const input = fixture(); input.state.jobs[0].started_at = time(-24 * 3600000);
  await verifyMain(input);
});

test('split quality evidence requires every producer and ages actual security jobs', async () => {
  const complete = fixture(); splitQuality(complete);
  await verifyMain(complete);
  for (const name of ['quality / go', 'quality / frontend', 'quality / scans',
    'quality / package', 'quality / docs-ste / ste-cli', 'quality / docs-ste / vale', ...Array.from({ length: 6 }, (_, shard) => `quality / native (${shard})`)]) {
    const missing = fixture(); splitQuality(missing);
    missing.state.jobs = missing.state.jobs.filter(job => job.name !== name);
    await assert.rejects(verifyMain(missing), /exact-attempt job|successful step/);
  }
  for (const name of ['quality / go', 'quality / frontend', 'quality / scans',
    'quality / docs-ste / ste-cli', 'quality / docs-ste / vale']) {
    const stale = fixture(); splitQuality(stale);
    stale.state.jobs[0].started_at = time(-60000);
    stale.state.jobs.find(job => job.name === name).started_at = time(-24 * 3600000 - 1);
    await assert.rejects(verifyMain(stale), /stale/);
  }
});

test('release reuse requires successful STE producers from the current attempt', async () => {
  const complete = fixture(); splitQuality(complete); complete.state.run.run_attempt = 2;
  for (const job of complete.state.jobs) job.run_attempt = 2;
  await verifyMain(complete);
  for (const name of ['quality / docs-ste / ste-cli', 'quality / docs-ste / vale']) {
    for (const change of [
      job => { job.run_attempt = 1; },
      job => { job.conclusion = 'failure'; },
      job => { job.conclusion = 'cancelled'; },
      job => { job.conclusion = 'skipped'; },
      job => { job.head_sha = OTHER; },
    ]) {
      const input = fixture(); splitQuality(input); input.state.run.run_attempt = 2;
      for (const job of input.state.jobs) job.run_attempt = 2;
      change(input.state.jobs.find(job => job.name === name));
      await assert.rejects(verifyMain(input), /exact-attempt job/);
    }
  }
});

test('reruns need every required job from the successful current attempt', async () => {
  const input = fixture(); input.state.run.run_attempt = 2;
  // Prior-success jobs omitted by a failed-jobs rerun must not be inherited.
  input.state.jobs = [input.state.jobs[3]]; input.state.jobs[0].run_attempt = 2;
  await assert.rejects(verifyMain(input), /exact-attempt job/);
  const all = fixture(); all.state.run.run_attempt = 2;
  for (const job of all.state.jobs) job.run_attempt = 2;
  await verifyMain(all);
});

test('attempt-specific endpoint supplies binding when the optional job attempt field is absent', async () => {
  const input = fixture(); input.state.run.run_attempt = 2;
  for (const job of input.state.jobs) delete job.run_attempt;
  assert.deepEqual(await verifyMain(input), { run_id: 456, run_attempt: 2 });
  const jobQuery = input.state.queries.find(params => params.attempt_number !== undefined);
  assert.equal(jobQuery.attempt_number, 2);
  for (const attempt of [1, 3, null, '2']) {
    const wrong = fixture(); wrong.state.run.run_attempt = 2;
    for (const job of wrong.state.jobs) delete job.run_attempt;
    wrong.state.jobs[0].run_attempt = attempt;
    await assert.rejects(verifyMain(wrong), /exact-attempt job/);
  }
});

test('rerunning main during evidence inspection rejects changing attempt or status', async () => {
  for (const change of ['attempt', 'status']) {
    const input = fixture(); let reads = 0;
    input.github.rest.actions.getWorkflowRun = async () => {
      const run = structuredClone(input.state.run);
      if (++reads > 1 && change === 'attempt') run.run_attempt++;
      if (reads > 1 && change === 'status') run.status = 'in_progress';
      return { data: run };
    };
    await assert.rejects(verifyMain(input), /changed while checking/);
  }
});

test('main advancement requires the exact frozen candidate and is rechecked after waiting', async () => {
  const frozen = fixture(); frozen.state.main = OTHER; frozen.state.frozen = SHA;
  await verifyMain(frozen);
  const changed = fixture(); changed.state.main = OTHER; changed.state.frozen = OTHER;
  await assert.rejects(verifyMain(changed), /prepared branch has another SHA/);
  const duringWait = fixture(); duringWait.state.run.status = 'in_progress';
  duringWait.sleep = async () => {
    duringWait.state.main = OTHER; duringWait.state.frozen = OTHER;
    duringWait.state.run.status = 'completed';
  };
  await assert.rejects(verifyMain(duringWait), /prepared branch has another SHA/);
});

test('published retry after branch cleanup requires complete immutable original-SHA assets', async () => {
  const input = fixture(); input.state.main = OTHER;
  input.state.release = { tag_name: `v${VERSION}`, immutable: true, draft: false, prerelease: false,
    assets: [`gpu-workload-supervisor-v${VERSION}.tar.gz`,
      `gpu-workload-supervisor_${VERSION}_linux_amd64.tar.gz`,
      `gpu-workload-supervisor_${VERSION}_linux_arm64.tar.gz`,
      `gpu-workload-supervisor_${VERSION}_linux_amd64.deb`,
      `gpu-workload-supervisor_${VERSION}_linux_arm64.deb`, 'SHA256SUMS', 'sbom.cdx.json']
      .map(name => ({ name, state: 'uploaded', digest: `sha256:${'e'.repeat(64)}` })) };
  await verifyMain(input);
  input.state.release.immutable = false;
  await assert.rejects(verifyMain(input), /not verified immutable/);
});

test('invalid dispatch context and API failures cannot authorize release', async () => {
  const input = fixture(); input.context.eventName = 'push';
  await assert.rejects(verifyCandidate(input), /dispatch from main/);
  const forbidden = fixture(); forbidden.github.rest.actions.getWorkflow = async () => {
    throw Object.assign(new Error('Forbidden'), { status: 403 });
  };
  await assert.rejects(verifyMain(forbidden), /Forbidden/);
});

test('protected runtime wrapper executes approved verifier bytes against real behavior', async () => {
  const input = fixture();
  for (const job of input.state.jobs) {
    job.started_at = new Date(Date.now() - 3600000).toISOString();
    job.completed_at = new Date(Date.now() - 60000).toISOString();
  }
  const previous = process.env.VERSION;
  process.env.VERSION = VERSION;
  try {
    const AsyncFunction = Object.getPrototypeOf(async function () {}).constructor;
    await new AsyncFunction('require', 'github', 'context', 'core', VALIDATION_SCRIPT)(
      require, input.github, input.context, input.core,
    );
    assert.equal(input.state.reads.length, 2);
    assert.match(input.state.logs.at(-1), /Reusing main run 456 attempt 1/);
  } finally {
    if (previous === undefined) delete process.env.VERSION;
    else process.env.VERSION = previous;
  }
});

test('overwritten workspace verifier cannot turn the protected runtime gate into a no-op', () => {
  const dir = mkdtempSync(join(tmpdir(), 'release-verifier-'));
  try {
    const scripts = join(dir, '.github', 'scripts'); mkdirSync(scripts, { recursive: true });
    writeFileSync(join(scripts, 'release-publication.cjs'), readFileSync(join(__dirname, 'release-publication.cjs')));
    const filename = join(scripts, 'release-validation.cjs');
    writeFileSync(filename, 'module.exports = { verifyMain: async () => {} };\n');
    const source = `const AsyncFunction = Object.getPrototypeOf(async function () {}).constructor;
new AsyncFunction('require', 'github', 'context', 'core', ${JSON.stringify(VALIDATION_SCRIPT)})(
  require, {}, {}, {},
).catch(error => { console.error(error.message); process.exitCode = 1; });`;
    assert.throws(() => execFileSync(process.execPath, ['-e', source], { cwd: dir, stdio: 'pipe' }),
      error => error.stderr.toString().includes('verifier bytes changed in the workspace'));
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

test('runtime gate executes captured bytes even if workspace changes after hash verification', () => {
  const dir = mkdtempSync(join(tmpdir(), 'release-verifier-race-'));
  try {
    const scripts = join(dir, '.github', 'scripts'); mkdirSync(scripts, { recursive: true });
    for (const name of ['release-validation.cjs', 'release-publication.cjs']) {
      writeFileSync(join(scripts, name), readFileSync(join(__dirname, name)));
    }
    symlinkSync(resolve(__dirname, 'node_modules'), join(scripts, 'node_modules'), 'dir');
    const source = `const fs = require('node:fs'); const vm = require('node:vm');
const guardedRequire = name => name === 'node:vm' ? {
  compileFunction(...args) {
    fs.writeFileSync('.github/scripts/release-validation.cjs', 'module.exports = { verifyMain: async () => {} };');
    return vm.compileFunction(...args);
  }
} : require(name);
const AsyncFunction = Object.getPrototypeOf(async function () {}).constructor;
new AsyncFunction('require', 'github', 'context', 'core', ${JSON.stringify(VALIDATION_SCRIPT)})(
  guardedRequire, {}, {}, {},
).catch(error => { console.error(error.message); process.exitCode = 1; });`;
    assert.throws(() => execFileSync(process.execPath, ['-e', source], { cwd: dir, stdio: 'pipe' }),
      error => error.stderr.toString().includes('Release requires dispatch from main'));
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

test('the validation trust chain never executes follow-up mutation code or npm semver', () => {
  const dir = mkdtempSync(join(tmpdir(), 'release-validation-dependency-'));
  try {
    const scripts = join(dir, '.github', 'scripts'); mkdirSync(scripts, { recursive: true });
    for (const name of ['release-validation.cjs', 'release-publication.cjs']) {
      writeFileSync(join(scripts, name), readFileSync(join(__dirname, name)));
    }
    const exit = "require('node:fs').writeFileSync('dependency-executed', 'true'); process.exit(0);";
    writeFileSync(join(scripts, 'release-follow-up.cjs'),
      `if (process.env.GITHUB_ACTIONS === 'true' && process.env.VERSION) { ${exit} }`);
    const npm = join(scripts, 'node_modules', 'semver'); mkdirSync(npm, { recursive: true });
    writeFileSync(join(npm, 'index.js'), exit);
    const source = `const AsyncFunction = Object.getPrototypeOf(async function () {}).constructor;
new AsyncFunction('require', 'github', 'context', 'core', ${JSON.stringify(VALIDATION_SCRIPT)})(
  require, {}, {}, {},
).catch(error => { console.error(error.message); process.exitCode = 1; });`;
    assert.throws(() => execFileSync(process.execPath, ['-e', source], {
      cwd: dir, stdio: 'pipe', env: { ...process.env, GITHUB_ACTIONS: 'true', VERSION },
    }), error => error.stderr.toString().includes('Release requires dispatch from main'));
    assert.throws(() => readFileSync(join(dir, 'dependency-executed')), { code: 'ENOENT' });
    // A modified helper must fail before its top-level code can exit successfully.
    writeFileSync(join(scripts, 'release-publication.cjs'), exit);
    assert.throws(() => execFileSync(process.execPath, ['-e', source], { cwd: dir, stdio: 'pipe' }),
      error => error.stderr.toString().includes('publication helper bytes changed in the workspace'));
    assert.throws(() => readFileSync(join(dir, 'dependency-executed')), { code: 'ENOENT' });
  } finally { rmSync(dir, { recursive: true, force: true }); }
});
