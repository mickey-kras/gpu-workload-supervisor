const test = require('node:test');
const assert = require('node:assert/strict');
const { reviewedHead, validReview } = require('./policy-admission.cjs');

const SHA = 'a'.repeat(40);
const BASE = 'b'.repeat(40);
const owner = { login: 'mickey-kras', id: 24433677, type: 'User' };
const review = { id: 1, user: owner, commit_id: SHA, state: 'APPROVED' };
const rule = { type: 'pull_request', ruleset_id: 123, ruleset_source_type: 'Repository',
  ruleset_source: 'mickey-kras/gpu-workload-supervisor', parameters: {
  require_code_owner_review: true, dismiss_stale_reviews_on_push: true,
  require_last_push_approval: true, required_approving_review_count: 1,
} };

function fixture() {
  const pull = { number: 1, state: 'open', user: { type: 'Bot', id: 456 },
    head: { sha: SHA }, base: { sha: BASE, ref: 'main' } };
  const state = { pull, current: pull, reviews: [review], rules: [rule],
    ruleset: { id: 123, source_type: 'Repository', source: 'mickey-kras/gpu-workload-supervisor',
      updated_at: '2026-10-23T00:00:00Z', enforcement: 'active', target: 'branch', bypass_actors: [], rules: [rule] },
    owners: '/.github/ @mickey-kras\n' };
  let calls = 0;
  const github = {
    rest: {
      pulls: { get: async () => ({ data: calls++ ? state.current : state.pull }), listReviews() {} },
      repos: { getContent: async params => {
        assert.equal(params.ref, BASE);
        return { data: { type: 'file', encoding: 'base64', content: Buffer.from(state.owners).toString('base64') } };
      } },
    },
    request: async () => ({ data: state.ruleset }),
    paginate: async route => typeof route === 'string' ? state.rules : state.reviews,
  };
  return { state, github, context: { repo: { owner: 'mickey-kras', repo: 'gpu-workload-supervisor' }, payload: { pull_request: pull } } };
}

test('admits only current owner approval with independently enforced review controls', async () => {
  const f = fixture();
  assert.equal(await reviewedHead(f.github, f.context), true);
});

const settingsReview = values => JSON.stringify({ repository: 'mickey-kras/gpu-workload-supervisor',
  ruleset_id: 123, updated_at: '2026-10-23T00:00:00Z', bypass_actors: [], ...values });

test('redacted bypasses need the exact current administrator no-bypass readback', async () => {
  const f = fixture();
  delete f.state.ruleset.bypass_actors;
  let rulePagination = false;
  f.github.paginate = async (route, params) => {
    if (typeof route === 'string') {
      assert.equal(params.per_page, 100);
      rulePagination = true;
      return f.state.rules;
    }
    return f.state.reviews;
  };
  for (const value of ['', '{}', 'null', '[]', '{', settingsReview({ repository: 'other/repo' }),
    settingsReview({ ruleset_id: 456 }), settingsReview({ ruleset_id: '123' }),
    settingsReview({ updated_at: '2026-10-22T00:00:00Z' }), settingsReview({ bypass_actors: [{}] })]) {
    assert.equal(await reviewedHead(f.github, f.context, value), false);
  }
  assert.equal(await reviewedHead(f.github, f.context, settingsReview()), true);
  assert.equal(rulePagination, true);
  f.state.ruleset.updated_at = '2026-10-23T00:00:01Z';
  assert.equal(await reviewedHead(f.github, f.context, settingsReview()), false);
  f.state.rules = [];
  assert.equal(await reviewedHead(f.github, f.context), false);
});

test('readback cannot override visible bypasses or a disabled or weakened source', async () => {
  for (const change of [{ bypass_actors: [{ actor_id: 456, actor_type: 'Integration', bypass_mode: 'pull_request' }] },
    { bypass_actors: null }, { bypass_actors: {} }, { bypass_actors: '' }, { enforcement: 'disabled' },
    { enforcement: 'evaluate' }, { id: 456 }, { source_type: 'Organization' }, { source: 'other/repo' },
    { target: 'tag' }, { rules: [] }, { rules: [{ ...rule, parameters: { ...rule.parameters, require_last_push_approval: false } }] }]) {
    const f = fixture();
    Object.assign(f.state.ruleset, change);
    assert.equal(await reviewedHead(f.github, f.context, settingsReview()), false);
  }
  for (const change of [{ ruleset_id: 0 }, { ruleset_id: '123' }, { ruleset_source_type: 'Organization' },
    { ruleset_source: 'other/repo' }]) {
    const f = fixture();
    f.state.rules = [{ ...rule, ...change }];
    assert.equal(await reviewedHead(f.github, f.context, settingsReview()), false);
  }
});

test('rejects self-authored changes, stale reviews, dismissed reviews, bypasses and missing controls', async () => {
  for (const mutate of [
    s => { s.pull.user = owner; },
    s => { s.reviews = [{ ...review, commit_id: BASE }]; },
    s => { s.reviews.push({ ...review, id: 2, state: 'DISMISSED' }); },
    s => { s.reviews.push({ ...review, id: 2, state: 'CHANGES_REQUESTED' }); },
    s => { s.reviews = [{ ...review, user: { ...owner, id: 99 } }]; },
    s => { s.rules = []; },
    s => { s.ruleset = { ...s.ruleset, bypass_actors: [{ actor_id: 1 }] }; },
    s => { s.ruleset = { ...s.ruleset, enforcement: 'evaluate' }; },
    s => { s.owners += '/.github/scripts/ @someone-else\n'; },
    s => { s.current = { ...s.pull, head: { sha: BASE } }; },
    s => { s.current = { ...s.pull, base: { ...s.pull.base, sha: SHA } }; },
  ]) {
    const f = fixture(); mutate(f.state);
    assert.equal(await reviewedHead(f.github, f.context), false);
  }
});

test('comments do not revoke an approval but later decisive reviews do', () => {
  assert.equal(validReview([review, { ...review, id: 2, state: 'COMMENTED' }], SHA), true);
  assert.equal(validReview([{ ...review, id: 2, state: 'CHANGES_REQUESTED' }, review], SHA), false);
});

test('API failures cannot authorize an unknown digest', async () => {
  const f = fixture();
  f.github.request = async () => { throw new Error('unavailable'); };
  await assert.rejects(reviewedHead(f.github, f.context), /unavailable/);
});
