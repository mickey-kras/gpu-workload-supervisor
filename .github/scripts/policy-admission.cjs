const OWNER = { login: 'mickey-kras', id: 24433677 };
const POLICY_SOURCES = [
  '.github/CODEOWNERS', '.github/workflows/policy-guard.yml',
  '.github/scripts/policy-guard.cjs', '.github/scripts/policy-admission.cjs',
  '.github/scripts/policy-lockfiles.cjs', '.github/scripts/policy-release.cjs',
  '.github/scripts/policy-ci-parallel.cjs', '.github/scripts/policy-ci-reuse.cjs',
  '.github/scripts/policy-pr-scope.cjs', '.github/scripts/policy-action-pins.cjs',
  '.github/scripts/policy-dependabot.cjs',
];

function validReview(reviews, sha) {
  const decisive = reviews.filter(review => Number.isSafeInteger(review.id) && review.user?.login === OWNER.login &&
    review.user?.id === OWNER.id && review.user?.type === 'User' &&
    ['APPROVED', 'CHANGES_REQUESTED', 'DISMISSED'].includes(review.state));
  decisive.sort((a, b) => b.id - a.id);
  return decisive[0]?.state === 'APPROVED' && decisive[0].commit_id === sha;
}

function protectedReview(rule) {
  const settings = rule?.parameters;
  return rule?.type === 'pull_request' && settings?.require_code_owner_review === true &&
    settings.dismiss_stale_reviews_on_push === true && settings.require_last_push_approval === true &&
    settings.required_approving_review_count >= 1;
}

async function reviewedHead(github, context) {
  const params = { ...context.repo, pull_number: context.payload.pull_request.number };
  const expected = context.payload.pull_request;
  const { data: pull } = await github.rest.pulls.get(params);
  if (pull.state !== 'open' || pull.head?.sha !== expected.head.sha || pull.base?.sha !== expected.base.sha ||
      pull.user?.type !== 'Bot' || !Number.isSafeInteger(pull.user.id) || pull.user.id === OWNER.id) return false;
  let owners;
  try {
    ({ data: owners } = await github.rest.repos.getContent({
      ...context.repo, path: '.github/CODEOWNERS', ref: pull.base.sha,
    }));
  } catch (error) {
    if (error.status === 404) return false;
    throw error;
  }
  if (owners.type !== 'file' || owners.encoding !== 'base64' || typeof owners.content !== 'string') return false;
  const lines = Buffer.from(owners.content, 'base64').toString('utf8').split('\n')
    .map(line => line.trim()).filter(line => line && !line.startsWith('#'));
  if (lines.length !== 1 || lines[0] !== '/.github/ @mickey-kras') return false;
  const rules = await github.paginate('GET /repos/{owner}/{repo}/rules/branches/{branch}', {
    ...context.repo, branch: pull.base.ref, per_page: 100,
  });
  const candidates = Array.isArray(rules) ? rules.filter(protectedReview) : [];
  let enforced = false;
  for (const rule of candidates) {
    if (!Number.isSafeInteger(rule.ruleset_id)) continue;
    const { data: ruleset } = await github.request('GET /repos/{owner}/{repo}/rulesets/{ruleset_id}', {
      ...context.repo, ruleset_id: rule.ruleset_id,
    });
    const bypasses = ruleset.bypass_actors;
    if (ruleset.enforcement === 'active' && ruleset.target === 'branch' &&
        (bypasses === undefined || (Array.isArray(bypasses) && bypasses.length === 0)) &&
        ruleset.rules?.some(protectedReview)) enforced = true;
  }
  if (!enforced) return false;
  const reviews = await github.paginate(github.rest.pulls.listReviews, { ...params, per_page: 100 });
  if (!validReview(reviews, pull.head.sha)) return false;
  const { data: current } = await github.rest.pulls.get(params);
  return current.state === 'open' && current.head?.sha === pull.head.sha && current.base?.sha === pull.base.sha;
}

module.exports = { POLICY_SOURCES, reviewedHead, validReview, protectedReview };
