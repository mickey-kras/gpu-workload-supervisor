const assert = require('node:assert/strict');

const RELEASE_APP_ACTOR_ID = 4956768;
const RELEASE_RULES = [
  ['Release branch creation', 'branch', 'refs/heads/release/*', ['creation'], true],
  ['Protect release branches', 'branch', 'refs/heads/release/*',
    ['non_fast_forward', 'pull_request', 'required_status_checks', 'code_scanning', 'copilot_code_review'], false],
  ['Release branch deletion', 'branch', 'refs/heads/release/*', ['deletion'], true],
  ['Release tag creation', 'tag', 'refs/tags/v*', ['creation'], true],
  ['Protect release tags', 'tag', 'refs/tags/v*', ['update', 'deletion', 'non_fast_forward'], false],
];

function requireValue(condition, message) {
  if (!condition) throw new Error(message);
}

function reviewedSettings(value) {
  let review;
  try { review = JSON.parse(value || 'null'); }
  catch { throw new Error('Invalid RELEASE_SETTINGS_REVIEW JSON'); }
  requireValue(review?.app_id === RELEASE_APP_ACTOR_ID && review.immutable_releases === true &&
    review.rulesets && !Array.isArray(review.rulesets) &&
    Object.keys(review.rulesets).length === RELEASE_RULES.length,
  'Set RELEASE_SETTINGS_REVIEW to the owner-reviewed App, immutable release, and five ruleset revisions');
  return review;
}

function verifyRule(rule, spec, review) {
  const [name, target, include, types, bypass] = spec;
  requireValue(rule.name === name && rule.target === target && rule.enforcement === 'active',
    `${name}: missing active protection`);
  assert.deepEqual(rule.conditions?.ref_name, { exclude: [], include: [include] },
    `${name}: changed ref targets`);
  const reviewedAt = review.rulesets[rule.id];
  requireValue(typeof rule.updated_at === 'string' && typeof reviewedAt === 'string' &&
    Date.parse(reviewedAt) === Date.parse(rule.updated_at),
  `${name}: update RELEASE_SETTINGS_REVIEW after reviewing this revision`);
  const expectedActor = bypass ? [{ actor_id: RELEASE_APP_ACTOR_ID,
    actor_type: 'Integration', bypass_mode: 'always' }] : [];
  if (Object.hasOwn(rule, 'bypass_actors')) {
    assert.deepEqual(rule.bypass_actors, expectedActor, `${name}: unexpected bypass actors`);
  }
  requireValue(Array.isArray(rule.rules) && rule.rules.length === types.length &&
    types.every(type => rule.rules.some(item => item.type === type)) &&
    rule.rules.every(item => types.includes(item.type)), `${name}: changed protections`);
  if (name !== 'Protect release branches') return;
  const pull = rule.rules.find(item => item.type === 'pull_request').parameters;
  requireValue(pull?.required_review_thread_resolution === true &&
    JSON.stringify(pull.allowed_merge_methods) === '["squash"]' &&
    pull.required_approving_review_count === 0, `${name}: changed review policy`);
  const checks = rule.rules.find(item => item.type === 'required_status_checks').parameters;
  requireValue(checks?.strict_required_status_checks_policy === true &&
    checks.do_not_enforce_on_create === true &&
    ['quality / checks', 'aislop / aislop status', 'codeql / analyze', 'guard'].every(context =>
      checks.required_status_checks?.some(check => check.context === context && check.integration_id === 15368)),
  `${name}: changed required checks`);
  const scanning = rule.rules.find(item => item.type === 'code_scanning').parameters?.code_scanning_tools;
  requireValue([
    ['CodeQL', 'high_or_higher'], ['Trivy', 'critical'], ['aislop', 'high_or_higher'],
  ].every(([tool, threshold]) => scanning?.some(entry => entry.tool === tool &&
    entry.security_alerts_threshold === threshold && entry.alerts_threshold === 'errors')),
    `${name}: changed code scanning`);
}

async function verify({ github, context, review: value }) {
  const review = reviewedSettings(value);
  const summaries = await github.paginate(github.rest.repos.getRepoRulesets,
    { ...context.repo, per_page: 100 });
  const names = summaries.filter(item => item.name === 'Enforce work branch names');
  requireValue(names.length === 1, 'Work branch naming protection is missing');
  const { data: naming } = await github.rest.repos.getRepoRuleset({
    ...context.repo, ruleset_id: names[0].id,
  });
  requireValue(naming.enforcement === 'active' &&
    naming.conditions?.ref_name?.exclude?.includes('refs/heads/release/*'),
  'Release branches must be excluded from the work branch naming rule');
  const reviewedIds = new Set();
  for (const spec of RELEASE_RULES) {
    const matches = summaries.filter(item => item.name === spec[0]);
    requireValue(matches.length === 1, `Configure exactly one ${spec[0]} ruleset`);
    const { data: rule } = await github.rest.repos.getRepoRuleset({
      ...context.repo, ruleset_id: matches[0].id,
    });
    verifyRule(rule, spec, review);
    reviewedIds.add(String(rule.id));
  }
  requireValue(Object.keys(review.rulesets).every(id => reviewedIds.has(id)),
    'RELEASE_SETTINGS_REVIEW contains an obsolete ruleset');
}

module.exports = { verify, verifyRule, reviewedSettings, RELEASE_RULES };
