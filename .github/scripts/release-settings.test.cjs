const test = require('node:test');
const assert = require('node:assert/strict');
const { verify, RELEASE_RULES } = require('./release-settings.cjs');

function fixture() {
  const rules = RELEASE_RULES.map(([name, target, include, types, bypass], index) => ({
    id: index + 100, name, target, enforcement: 'active',
    updated_at: `2026-09-29T12:47:5${index}.000-07:00`,
    conditions: { ref_name: { exclude: [], include: [include] } },
    bypass_actors: bypass ? [{ actor_id: 4956768, actor_type: 'Integration', bypass_mode: 'always' }] : [],
    rules: types.map(type => ({ type })),
  }));
  const branch = rules.find(rule => rule.name === 'Protect release branches');
  branch.rules.find(rule => rule.type === 'pull_request').parameters = {
    required_review_thread_resolution: true, required_approving_review_count: 0,
    allowed_merge_methods: ['squash'],
  };
  branch.rules.find(rule => rule.type === 'required_status_checks').parameters = {
    strict_required_status_checks_policy: true, do_not_enforce_on_create: true,
    required_status_checks: ['quality / checks', 'aislop / aislop status',
      'codeql / analyze', 'guard'].map(context => ({ context, integration_id: 15368 })),
  };
  branch.rules.find(rule => rule.type === 'code_scanning').parameters = {
    code_scanning_tools: [['CodeQL', 'high_or_higher'], ['Trivy', 'critical'],
      ['aislop', 'high_or_higher']].map(([tool, threshold]) => ({ tool,
      security_alerts_threshold: threshold, alerts_threshold: 'errors' })),
  };
  rules.push({ id: 200, name: 'Enforce work branch names', enforcement: 'active',
    conditions: { ref_name: { exclude: ['refs/heads/release/*'] } } });
  const review = JSON.stringify({ app_id: 4956768, immutable_releases: true,
    rulesets: Object.fromEntries(rules.slice(0, 5).map(rule => [rule.id, rule.updated_at])) });
  const github = {
    request: async () => ({ data: { enabled: true } }),
    paginate: async () => rules.map(({ id, name }) => ({ id, name })),
    rest: { repos: { getRepoRulesets() {},
      getRepoRuleset: async ({ ruleset_id }) => ({ data: rules.find(rule => rule.id === ruleset_id) }),
    } },
  };
  return { rules, review, github, context: { repo: { owner: 'mickey-kras', repo: 'gpu-workload-supervisor' } } };
}

test('accepts the five reviewed active release rules and immutable releases', async () => {
  await verify(fixture());
});

test('rejects a changed release App bypass', async () => {
  const input = fixture();
  input.rules[0].bypass_actors[0].actor_id = 1;
  await assert.rejects(verify(input), /unexpected bypass actors/);
});

test('rejects a ruleset revision newer than the owner review', async () => {
  const input = fixture();
  input.rules[3].updated_at = '2026-09-29T13:00:00.000-07:00';
  await assert.rejects(verify(input), /update RELEASE_SETTINGS_REVIEW/);
});

test('rejects missing required release branch checks', async () => {
  const input = fixture();
  input.rules[1].rules.find(rule => rule.type === 'required_status_checks')
    .parameters.required_status_checks.pop();
  await assert.rejects(verify(input), /changed required checks/);
});

test('rejects a weaker code scanning threshold', async () => {
  const input = fixture();
  input.rules[1].rules.find(rule => rule.type === 'code_scanning')
    .parameters.code_scanning_tools[0].security_alerts_threshold = 'critical';
  await assert.rejects(verify(input), /changed code scanning/);
});

test('rejects disabled release immutability', async () => {
  const input = fixture();
  input.github.request = async () => ({ data: { enabled: false } });
  await assert.rejects(verify(input), /Enable immutable releases/);
});

test('uses exact reviewed revisions if the API redacts bypass actors', async () => {
  const input = fixture();
  for (const rule of input.rules.slice(0, 5)) delete rule.bypass_actors;
  await verify(input);
  input.rules[2].updated_at = '2026-09-29T13:00:00.000-07:00';
  await assert.rejects(verify(input), /update RELEASE_SETTINGS_REVIEW/);
});
