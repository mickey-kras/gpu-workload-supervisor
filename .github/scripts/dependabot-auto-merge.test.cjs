const test = require('node:test');
const assert = require('node:assert/strict');
const { trustedPull, eligible, readDependencies } = require('./dependabot-auto-merge.cjs');

const bot = { login: 'dependabot[bot]', id: 49699333 };
const pull = {
  user: bot, state: 'open', draft: false,
  head: { ref: 'dependabot/go_modules/sqlite-1.60.1', repo: { full_name: 'owner/repo' } },
  base: { ref: 'main' },
};
const commits = [{ author: bot, commit: { verification: { verified: true } } }];
const update = {
  updateType: 'version-update:semver-patch',
  prevVersion: '1.60.0', newVersion: '1.60.1', compatScore: 75,
};

test('only the exact bot identity and same-repository branch are trusted', () => {
  assert.equal(trustedPull(pull, 'owner/repo', 'main'), true);
  assert.equal(trustedPull({ ...pull, user: { ...bot, id: 1 } }, 'owner/repo', 'main'), false);
  assert.equal(trustedPull({ ...pull, head: { ...pull.head, repo: { full_name: 'fork/repo' } } },
    'owner/repo', 'main'), false);
  assert.equal(trustedPull({ ...pull, draft: true }, 'owner/repo', 'main'), false);
});

test('every update must be minor or patch with a known score of at least 75', () => {
  assert.equal(eligible(commits, [update]), true);
  assert.equal(eligible(commits, [update, { ...update, compatScore: 74 }]), false);
  assert.equal(eligible(commits, [{ ...update, updateType: 'version-update:semver-major' }]), false);
  assert.equal(eligible(commits, [{ ...update, compatScore: null }]), false);
  assert.equal(eligible([], [update]), false);
  assert.equal(eligible([{ ...commits[0], author: { login: 'user', id: 1 } }], [update]), false);
  assert.equal(eligible([{ ...commits[0], commit: { verification: { verified: false } } }], [update]), false);
});

test('metadata parser rejects missing or incomplete action output', () => {
  assert.deepEqual(readDependencies('updated-dependencies-json<<END\n[]\nEND\n'), []);
  assert.throws(() => readDependencies(''), /missing/);
  assert.throws(() => readDependencies('updated-dependencies-json<<END\n[]\n'), /Incomplete/);
});

test('grouped CodeQL metadata preserves per-dependency eligibility checks', () => {
  const dependencies = ['init', 'analyze'].map(action => ({
    ...update, dependencyName: `github/codeql-action/${action}`,
    prevVersion: '4.38.1', newVersion: '4.38.2',
  }));
  const parse = entries => readDependencies(
    `updated-dependencies-json<<END\n${JSON.stringify(entries)}\nEND\n`);
  assert.equal(eligible(commits, parse(dependencies)), true);
  for (const invalid of [{ compatScore: 74 }, { compatScore: null },
    { updateType: 'version-update:semver-major' }]) {
    assert.equal(eligible(commits, parse([dependencies[0], { ...dependencies[1], ...invalid }])), false);
  }
});

function orchestration({ eventName = 'workflow_call', numbered = false } = {}) {
  const state = { pulls: [1], snapshots: {}, calls: [], metadata: [update], runs: [], merged: [] };
  const snapshot = number => structuredClone({ ...pull, number, head: { ...pull.head, sha: `head-${number}` } });
  const data = value => ({ data: value });
  const github = { rest: {
    repos: { get: async () => data({ default_branch: 'main' }),
      getBranch: async () => data({ commit: { sha: 'merged-tip' } }), listPullRequestsAssociatedWithCommit() {} },
    pulls: { list() {}, listCommits() {}, get: async ({ pull_number: number }) => {
      state.calls.push(['get', number]);
      const snapshots = state.snapshots[number];
      return data(snapshots?.length ? snapshots.shift() : snapshot(number));
    } },
    actions: { listWorkflowRuns() {}, createWorkflowDispatch: async params => state.calls.push(['dispatch', params]) },
  }, paginate: async (method, params) => {
    if (method === github.rest.pulls.list) return state.pulls.map(number => ({ number }));
    if (method === github.rest.pulls.listCommits) { state.calls.push(['commits', params.pull_number]); return commits; }
    if (method === github.rest.repos.listPullRequestsAssociatedWithCommit) return state.merged;
    if (method === github.rest.actions.listWorkflowRuns) return state.runs;
    throw new Error('Unexpected pagination');
  } };
  const input = { github, context: { repo: { owner: 'owner', repo: 'repo' }, eventName,
    ref: 'refs/heads/main', payload: numbered ? { pull_request: { number: 1 } } : {} },
  core: { info() {}, warning: text => state.calls.push(['warning', text]),
    setFailed: text => state.calls.push(['failed', text]) }, metadataPath: '/metadata', mainWorkflow: 'main.yml',
  metadata: candidate => {
    state.calls.push(['metadata', candidate.number]);
    if (state.metadata instanceof Error) throw state.metadata;
    return state.metadata;
  }, merge: (repository, number, options) => state.calls.push(['merge', repository, number, options]) };
  return { state, input, snapshot };
}

const mergeCalls = state => state.calls.filter(call => call[0] === 'merge');
const { run, ensureMainRun } = require('./dependabot-auto-merge.cjs');

test('run queues an eligible exact head with squash after revalidation', async () => {
  const { state, input } = orchestration({ eventName: 'pull_request_target', numbered: true });
  await run(input);
  assert.deepEqual(mergeCalls(state), [['merge', 'owner/repo', 1,
    ['--auto', '--squash', '--match-head-commit', 'head-1']]]);
  assert.deepEqual(state.calls.map(call => call[0]), ['get', 'commits', 'metadata', 'get', 'merge']);
});

test('run leaves ineligible updates and changed or newly untrusted heads unqueued', async () => {
  for (const scenario of ['score', 'head', 'draft']) {
    const { state, input, snapshot } = orchestration();
    if (scenario === 'score') state.metadata = [{ ...update, compatScore: 74 }];
    else {
      const current = snapshot(1);
      if (scenario === 'head') current.head.sha = 'new-head';
      else current.draft = true;
      state.snapshots[1] = [snapshot(1), current];
    }
    await run(input);
    assert.deepEqual(mergeCalls(state), [], scenario);
  }
});

test('run revokes prior bot authorization before metadata failure or ineligibility', async () => {
  for (const metadata of [new Error('metadata unavailable'), [{ ...update, compatScore: null }]]) {
    const { state, input, snapshot } = orchestration();
    const previous = snapshot(1);
    previous.auto_merge = { enabled_by: { login: 'github-actions[bot]' } };
    state.snapshots[1] = [previous]; state.metadata = metadata;
    await run(input);
    assert.deepEqual(mergeCalls(state), [['merge', 'owner/repo', 1, ['--disable-auto']]]);
    assert.ok(state.calls.findIndex(call => call[0] === 'merge') <
      state.calls.findIndex(call => call[0] === 'metadata'));
    assert.equal(state.calls.some(call => call[0] === 'failed'), metadata instanceof Error);
  }
});

test('run preserves human auto-merge decisions', async () => {
  const { state, input, snapshot } = orchestration();
  const manual = snapshot(1); manual.auto_merge = { enabled_by: { login: 'maintainer' } };
  state.snapshots[1] = [structuredClone(manual), manual];
  await run(input);
  assert.deepEqual(mergeCalls(state), []);
});

test('scheduled refresh isolates candidate failures and still recovers main validation', async () => {
  const { state, input } = orchestration({ eventName: 'schedule' });
  state.pulls = [1, 2];
  input.metadata = candidate => { if (candidate.number === 1) throw new Error('unavailable'); return [update]; };
  state.merged = [{ user: bot, merged_at: 'today', merge_commit_sha: 'merged-tip', base: { ref: 'main' } }];
  await run(input);
  assert.deepEqual(mergeCalls(state), [['merge', 'owner/repo', 2,
    ['--auto', '--squash', '--match-head-commit', 'head-2']]]);
  const dispatchIndex = state.calls.findIndex(call => call[0] === 'dispatch');
  assert.deepEqual(state.calls[dispatchIndex][1], { owner: 'owner', repo: 'repo', workflow_id: 'main.yml', ref: 'main' });
  assert.ok(dispatchIndex < state.calls.findIndex(call => call[0] === 'failed'));
});

test('main recovery dispatches only missing usable runs for a bot merge at current main', async () => {
  for (const conclusion of [undefined, null, 'success', 'failure', 'cancelled', 'skipped']) {
    const { state, input } = orchestration();
    state.merged = [{ user: bot, merged_at: 'today', merge_commit_sha: 'merged-tip', base: { ref: 'main' } }];
    if (conclusion !== undefined) state.runs = [{ head_sha: 'merged-tip', event: 'push', conclusion }];
    await ensureMainRun({ ...input, branch: 'main' });
    assert.equal(state.calls.some(call => call[0] === 'dispatch'),
      [undefined, 'cancelled', 'skipped'].includes(conclusion));
  }
  for (const mutate of [p => { p.user = { login: 'maintainer', id: 1 }; },
    p => { p.merge_commit_sha = 'old-tip'; }, p => { p.base.ref = 'other'; }, p => { p.merged_at = null; }]) {
    const { state, input } = orchestration();
    const merged = { user: bot, merged_at: 'today', merge_commit_sha: 'merged-tip', base: { ref: 'main' } };
    mutate(merged); state.merged = [merged];
    await ensureMainRun({ ...input, branch: 'main' });
    assert.equal(state.calls.some(call => call[0] === 'dispatch'), false);
  }
});

test('refresh rejects a nondefault branch before evaluating candidates', async () => {
  const { state, input } = orchestration({ eventName: 'workflow_dispatch' });
  input.context.ref = 'refs/heads/other';
  await assert.rejects(run(input), /Default branch required/);
  assert.deepEqual(state.calls, []);
});
