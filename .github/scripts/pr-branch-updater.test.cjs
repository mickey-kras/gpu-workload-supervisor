const test = require('node:test');
const assert = require('node:assert/strict');
const { run, validateContext } = require('./pr-branch-updater.cjs');

const OWNER = 'mickey-kras';
const REPO = 'gpu-workload-supervisor';
const HEAD = 'a'.repeat(40);
const MAIN = 'b'.repeat(40);
const NEW_HEAD = 'c'.repeat(40);

function fixture() {
  const pull = { number: 8, state: 'open', auto_merge: { merge_method: 'squash' },
    labels: [], mergeable: true, user: { login: OWNER, id: 1 },
    base: { ref: 'main', sha: HEAD },
    head: { sha: HEAD, repo: { full_name: `${OWNER}/${REPO}` } } };
  const state = { pull, listed: structuredClone(pull), ahead: 1, main: MAIN,
    reads: [], refs: [], comparisons: [], requests: [], updates: [], sleeps: [],
    failures: [], logs: [], table: null };
  const github = { rest: {
    pulls: {
      list() {},
      get: async params => {
        state.reads.push(params);
        return { data: structuredClone(state.pull) };
      },
    },
    git: { getRef: async params => {
      state.refs.push(params);
      return { data: { object: { sha: state.main } } };
    } },
    repos: { compareCommitsWithBasehead: async params => {
      state.comparisons.push(params);
      return { data: { ahead_by: state.ahead } };
    } },
  },
  paginate: async (method, params) => {
    assert.equal(method, github.rest.pulls.list);
    assert.deepEqual(params, { owner: OWNER, repo: REPO, state: 'open', base: 'main', per_page: 100 });
    return [structuredClone(state.listed)];
  },
  request: async (route, params) => {
    state.requests.push({ route, params });
    assert.equal(route, 'PUT /repos/{owner}/{repo}/pulls/{pull_number}/update-branch');
    if (params.expected_head_sha !== state.pull.head.sha) {
      throw Object.assign(new Error('head changed'), { status: 422 });
    }
    state.updates.push(params);
  } };
  const summary = {
    addHeading() { return this; },
    addTable(table) { state.table = table; return this; },
    async write() {},
  };
  return { github, state, context: { eventName: 'push', ref: 'refs/heads/main',
    repo: { owner: OWNER, repo: REPO } },
  core: { summary, info: value => state.logs.push(value), setFailed: value => state.failures.push(value) },
  sleep: async ms => { state.sleeps.push(ms); } };
}

const result = input => input.state.table[1][1];

test('updates opted-in work using the live main tip and exact PR head', async () => {
  for (const optIn of ['auto-merge', 'label', 'both']) {
    const input = fixture();
    if (optIn !== 'auto-merge') input.state.pull.labels = [{ name: 'auto-update' }];
    if (optIn === 'label') input.state.pull.auto_merge = null;
    // Even a list snapshot without opt-in must defer to the live PR.
    input.state.listed.auto_merge = null;
    input.state.listed.labels = [];
    await run(input);
    assert.equal(result(input), 'update requested', optIn);
    assert.deepEqual(input.state.refs, [{ owner: OWNER, repo: REPO, ref: 'heads/main' }]);
    assert.deepEqual(input.state.comparisons, [{ owner: OWNER, repo: REPO, basehead: `${HEAD}...${MAIN}` }]);
    assert.deepEqual(input.state.updates, [{ owner: OWNER, repo: REPO, pull_number: 8, expected_head_sha: HEAD }]);
    assert.deepEqual(input.state.failures, []);
  }
});

test('ordinary work and unrelated labels do not request updates or comparisons', async () => {
  for (const labels of [[], [{ name: 'auto-merge' }], [{ name: 'auto-update-later' }]]) {
    const input = fixture();
    input.state.pull.auto_merge = null;
    input.state.pull.labels = labels;
    await run(input);
    assert.equal(result(input), 'not opted in');
    assert.deepEqual(input.state.comparisons, []);
    assert.deepEqual(input.state.requests, []);
    assert.deepEqual(input.state.failures, []);
  }
});

test('live eligibility excludes forks, closed PRs, changed bases and deleted head repositories', async () => {
  for (const mutate of [
    pull => { pull.head.repo.full_name = 'other/fork'; },
    pull => { pull.state = 'closed'; },
    pull => { pull.base.ref = 'release/0.1.0'; },
    pull => { pull.head.repo = null; },
  ]) {
    const input = fixture(); mutate(input.state.pull);
    await run(input);
    assert.equal(result(input), 'ineligible');
    assert.equal(input.state.reads.length, 1);
    assert.deepEqual(input.state.requests, []);
    assert.deepEqual(input.state.comparisons, []);
  }
  const input = fixture(); input.state.listed.head.repo.full_name = 'other/fork';
  await run(input);
  assert.equal(result(input), 'ineligible');
  assert.deepEqual(input.state.reads, []);
});

test('current branches and conflicts remain untouched', async () => {
  const current = fixture(); current.state.ahead = 0;
  await run(current);
  assert.equal(result(current), 'current');
  assert.deepEqual(current.state.requests, []);
  const conflict = fixture(); conflict.state.pull.mergeable = false;
  await run(conflict);
  assert.equal(result(conflict), 'conflicting');
  assert.deepEqual(conflict.state.requests, []);
});

test('only the real Dependabot identity uses scheduled rebasing', async () => {
  const bot = fixture(); bot.state.pull.user = { login: 'dependabot[bot]', id: 49699333 };
  await run(bot);
  assert.equal(result(bot), 'managed by scheduled Dependabot rebasing');
  assert.deepEqual(bot.state.requests, []);
  for (const user of [{ login: 'dependabot[bot]', id: 1 }, { login: 'other', id: 49699333 }]) {
    const input = fixture(); input.state.pull.user = user;
    await run(input);
    assert.equal(result(input), 'update requested');
  }
});

test('removing either opt-in while mergeability is pending stops the next attempt', async () => {
  for (const optIn of ['auto-merge', 'label']) {
    const input = fixture(); input.state.pull.mergeable = null;
    if (optIn === 'label') {
      input.state.pull.auto_merge = null;
      input.state.pull.labels = [{ name: 'auto-update' }];
    }
    input.sleep = async ms => {
      input.state.sleeps.push(ms);
      input.state.pull.auto_merge = null;
      input.state.pull.labels = [];
      input.state.pull.mergeable = true;
    };
    await run(input);
    assert.equal(result(input), 'not opted in');
    assert.equal(input.state.reads.length, 2);
    assert.equal(input.state.comparisons.length, 1);
    assert.deepEqual(input.state.requests, []);
  }
});

test('closing or moving a PR to a fork or another base during retry stops updates', async () => {
  for (const mutate of [
    pull => { pull.state = 'closed'; },
    pull => { pull.head.repo.full_name = 'other/fork'; },
    pull => { pull.base.ref = 'release/0.1.0'; },
  ]) {
    const input = fixture(); input.state.pull.mergeable = null;
    input.sleep = async () => { mutate(input.state.pull); input.state.pull.mergeable = true; };
    await run(input);
    assert.equal(result(input), 'ineligible');
    assert.equal(input.state.reads.length, 2);
    assert.deepEqual(input.state.requests, []);
  }
});

test('retry recomputes comparison and expected head after concurrent commits', async () => {
  const input = fixture(); input.state.pull.mergeable = null;
  input.sleep = async () => {
    input.state.pull.head.sha = NEW_HEAD;
    input.state.main = HEAD;
    input.state.pull.mergeable = true;
  };
  await run(input);
  assert.deepEqual(input.state.comparisons.map(params => params.basehead), [`${HEAD}...${MAIN}`, `${NEW_HEAD}...${HEAD}`]);
  assert.equal(input.state.updates[0].expected_head_sha, NEW_HEAD);
  assert.equal(result(input), 'update requested');
});

test('a head changing between inspection and request fails expected-head protection', async () => {
  const input = fixture();
  input.github.rest.repos.compareCommitsWithBasehead = async () => {
    input.state.pull.head.sha = NEW_HEAD;
    return { data: { ahead_by: 1 } };
  };
  await run(input);
  assert.equal(input.state.requests[0].params.expected_head_sha, HEAD);
  assert.deepEqual(input.state.updates, []);
  assert.equal(result(input), 'unresolved: head changed');
  assert.deepEqual(input.state.failures, ['1 PR branch update(s) unresolved']);
});

test('unknown mergeability and invalid comparisons fail closed', async () => {
  const unknown = fixture(); unknown.state.pull.mergeable = null;
  await run(unknown);
  assert.equal(unknown.state.reads.length, 4);
  assert.deepEqual(unknown.state.sleeps, [5000, 5000, 5000]);
  assert.equal(result(unknown), 'unresolved: mergeability remained unknown after 4 attempts');
  assert.deepEqual(unknown.state.requests, []);
  assert.equal(unknown.state.failures.length, 1);
  for (const ahead of [-1, undefined, '1']) {
    const input = fixture(); input.state.ahead = ahead;
    await run(input);
    assert.equal(result(input), 'unresolved: invalid commit comparison');
    assert.deepEqual(input.state.requests, []);
    assert.equal(input.state.failures.length, 1);
  }
});

test('only push or dispatch on main can read or update PRs', async () => {
  for (const eventName of ['push', 'workflow_dispatch']) {
    assert.equal(validateContext({ eventName, ref: 'refs/heads/main' }), 'main');
  }
  for (const context of [
    { eventName: 'pull_request', ref: 'refs/heads/main' },
    { eventName: 'workflow_dispatch', ref: 'refs/heads/ci/test' },
    { eventName: 'push', ref: 'refs/tags/v0.1.0' },
  ]) {
    const input = fixture(); Object.assign(input.context, context);
    await assert.rejects(run(input), /push or dispatch on main/);
    assert.deepEqual(input.state.reads, []);
    assert.deepEqual(input.state.requests, []);
  }
});
