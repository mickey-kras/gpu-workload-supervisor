const test = require('node:test');
const assert = require('node:assert/strict');
const { reviewedHead, validReview } = require('./policy-admission.cjs');

const SHA = 'a'.repeat(40);
const BASE = 'b'.repeat(40);
const owner = { login: 'mickey-kras', id: 24433677, type: 'User' };
const review = { id: 1, user: owner, commit_id: SHA, state: 'APPROVED' };
const rule = { type: 'pull_request', ruleset_id: 123, parameters: {
  require_code_owner_review: true, dismiss_stale_reviews_on_push: true,
  require_last_push_approval: true, required_approving_review_count: 1,
} };

function fixture() {
  const pull = { number: 1, state: 'open', user: { type: 'Bot', id: 456 },
    head: { sha: SHA }, base: { sha: BASE, ref: 'main' } };
  const state = { pull, current: pull, reviews: [review], rules: [rule],
    ruleset: { enforcement: 'active', target: 'branch', bypass_actors: [], rules: [rule] },
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

test('read-only ruleset responses use effective rules and the administrator no-bypass contract', async () => {
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
  assert.equal(await reviewedHead(f.github, f.context), true);
  assert.equal(rulePagination, true);
  f.state.rules = [];
  assert.equal(await reviewedHead(f.github, f.context), false);
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
