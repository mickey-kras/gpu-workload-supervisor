const YAML = require('yaml');
const { canonical, normalizeActionSteps } = require('./policy-action-pins.cjs');

// Selected accident tripwires, not a proof of arbitrary JavaScript semantics.
// Trusted code execution, human review and behavioral tests remain the boundary.
// Include the trusted guard's local policy import graph, including pin normalization.
const DEPENDABOT_SOURCES = [
  '.github/scripts/dependabot-auto-merge.cjs', '.github/scripts/policy-guard.cjs',
  '.github/scripts/policy-dependabot.cjs', '.github/scripts/policy-action-pins.cjs',
  '.github/scripts/policy-release.cjs', '.github/scripts/policy-ci-parallel.cjs',
  '.github/scripts/policy-pr-scope.cjs', '.github/scripts/policy-ci-reuse.cjs',
  '.github/scripts/policy-lockfiles.cjs', '.github/scripts/policy-admission.cjs',
];
const compact = source => typeof source === 'string' ? source.replace(/\s+/g, ' ').trim() : '';

function inspectWorkflow(workflows, failures, checks) {
  const path = '.github/workflows/dependabot-auto-merge.yml';
  const workflow = workflows[path];
  const job = workflow?.jobs?.['enable-auto-merge'];
  const steps = job?.steps || [];
  checks.event(path, 'pull_request_target');
  checks.event(path, 'workflow_call');
  const activities = workflow?.on?.pull_request_target?.types;
  if (Object.keys(workflow?.on?.pull_request_target || {}).some(key => key !== 'types')) {
    failures.push('Dependabot PR trigger cannot filter required evaluations');
  }
  const requiredActivities = ['opened', 'synchronize', 'reopened', 'ready_for_review', 'closed'];
  if (!Array.isArray(activities) || activities.length !== requiredActivities.length ||
      !requiredActivities.every(activity => activities.includes(activity))) {
    failures.push('Dependabot lost required PR activities');
  }
  const queue = steps.find(step => step.name === 'Queue eligible verified updates');
  const script = `await require('./automation/.github/scripts/dependabot-auto-merge.cjs').run({
    github, context, core,
    metadataPath: require('node:path').resolve('metadata-action'),
    mainWorkflow: 'main.yml',
  });`;
  const concurrency = workflow?.concurrency;
  if (job?.if !== "github.event_name != 'pull_request_target' || github.event.pull_request.user.login == 'dependabot[bot]'" ||
      !concurrency || typeof concurrency !== 'object' || Array.isArray(concurrency) ||
      Object.keys(concurrency).length !== 2 ||
      !['group', 'cancel-in-progress'].every(key => Object.hasOwn(concurrency, key)) ||
      concurrency['cancel-in-progress'] !== false ||
      concurrency.group !== 'dependabot-auto-merge-${{ github.repository }}') {
    failures.push('Dependabot lost gated queue invocation or serialization');
  }
  const metadata = steps.find(step => step.name === 'Read pinned Dependabot metadata action');
  if (!/^[a-f0-9]{40}$/.test(metadata?.with?.ref || '')) failures.push('Dependabot lost pinned metadata ref');
  const expectedSteps = [
    { uses: 'step-security/harden-runner@' + 'a'.repeat(40), with: { 'egress-policy': 'audit' } },
    { name: 'Read trusted automation', uses: 'actions/checkout@' + 'a'.repeat(40),
      with: { ref: '${{ github.workflow_sha }}', 'persist-credentials': false,
        path: 'automation', 'sparse-checkout': '.github/scripts' } },
    { name: 'Read pinned Dependabot metadata action', uses: 'actions/checkout@' + 'a'.repeat(40),
      with: { repository: 'dependabot/fetch-metadata', ref: metadata?.with?.ref,
        'persist-credentials': false, path: 'metadata-action', 'sparse-checkout': 'dist' } },
    { name: 'Queue eligible verified updates', uses: 'actions/github-script@' + 'a'.repeat(40),
      env: { GH_TOKEN: '${{ github.token }}' }, with: { script: compact(script) } },
  ];
  for (const { name } of expectedSteps.slice(1, 3)) {
    if (steps.findIndex(step => step.name === name) >= steps.indexOf(queue)) {
      failures.push(`Dependabot checkout must precede queue: ${name}`);
    }
  }
  const executableSteps = steps.map(step => step.name === 'Queue eligible verified updates' ?
    { ...step, with: { ...step.with, script: compact(step.with?.script) } } : step);
  if (JSON.stringify(canonical(normalizeActionSteps(executableSteps))) !==
      JSON.stringify(canonical(normalizeActionSteps(expectedSteps))) ||
      Object.keys(workflow || {}).some(key => !['name', 'on', 'permissions', 'concurrency', 'jobs'].includes(key)) ||
      Object.keys(workflow?.on || {}).some(key => !['pull_request_target', 'workflow_call'].includes(key)) ||
      workflow?.on?.workflow_call !== null ||
      Object.keys(workflow?.jobs || {}).length !== 1 ||
      Object.keys(job || {}).some(key => !['name', 'if', 'runs-on', 'timeout-minutes', 'steps', 'permissions'].includes(key)) ||
      job?.['runs-on'] !== 'ubuntu-24.04' || job?.['timeout-minutes'] !== 15) {
    failures.push('Dependabot lost trusted executable workflow structure');
  }
}

// Policy intentionally supports a bounded daily cron subset, not arbitrary cron syntax.
function dailyCron(value) {
  const fields = typeof value === 'string' && value.trim().match(/^(\d{1,2}) +(\d{1,2}) +\* +\* +\*$/);
  return Boolean(fields && Number(fields[1]) <= 59 && Number(fields[2]) <= 23);
}

function supportedSchedule(schedule) {
  if (!schedule || typeof schedule !== 'object' || Array.isArray(schedule)) return false;
  if (['daily', 'weekly', 'monthly'].includes(schedule.interval)) return Object.keys(schedule).length === 1;
  return schedule.interval === 'cron' && Object.keys(schedule).length === 2 && dailyCron(schedule.cronjob);
}

function inspectRecovery(workflows, failures, checks) {
  const workflow = workflows['.github/workflows/dependabot-auto-merge.yml'];
  const refresh = '.github/workflows/dependabot-auto-merge-refresh.yml';
  checks.event(refresh, 'workflow_dispatch');
  checks.event(refresh, 'schedule');
  checks.job(refresh, 'refresh', './.github/workflows/dependabot-auto-merge.yml');
  const recovery = workflows[refresh];
  if (Object.keys(recovery || {}).some(key => !['name', 'on', 'permissions', 'jobs'].includes(key)) ||
      Object.keys(recovery?.on || {}).some(key => !['workflow_dispatch', 'schedule'].includes(key)) ||
      recovery?.on?.workflow_dispatch !== null || Object.keys(recovery?.jobs || {}).length !== 1 ||
      Object.keys(recovery?.jobs?.refresh || {}).some(key => !['name', 'uses', 'permissions'].includes(key))) {
    failures.push('Dependabot lost trusted recovery workflow structure');
  }
  const schedules = recovery?.on?.schedule;
  if (!Array.isArray(schedules) || !schedules.length || !schedules.every(item =>
    item && typeof item === 'object' && !Array.isArray(item) &&
      Object.keys(item).length === 1 && Object.hasOwn(item, 'cron') && dailyCron(item.cron))) {
    failures.push('Dependabot lost scheduled recovery');
  }
  for (const document of [workflow, workflows[refresh]]) {
    for (const scope of [document, ...Object.values(document?.jobs || {})]) {
      if (scope && Object.hasOwn(scope, 'name') && typeof scope.name !== 'string') {
        failures.push('Dependabot workflow and job names must be strings');
      }
      if (scope !== document && !Object.hasOwn(scope, 'permissions')) continue;
      const permissions = scope?.permissions;
      if (!permissions || Object.keys(permissions).length !== 3 ||
          !['contents', 'pull-requests', 'actions'].every(key => permissions[key] === 'write')) {
        failures.push('Dependabot lost required automation permissions');
      }
    }
  }
}

function supportedCooldown(cooldown) {
  return cooldown && typeof cooldown === 'object' && !Array.isArray(cooldown) &&
    Object.keys(cooldown).length === 1 && Object.hasOwn(cooldown, 'default-days') &&
    Number.isInteger(cooldown['default-days']) &&
    cooldown['default-days'] >= 1 && cooldown['default-days'] <= 90;
}

function supportedGroups(groups, ecosystem) {
  if (ecosystem !== 'github-actions' || !groups || typeof groups !== 'object' || Array.isArray(groups)) return false;
  const group = groups.codeql;
  return Object.keys(groups).length === 1 && Object.hasOwn(groups, 'codeql') && group && typeof group === 'object' && !Array.isArray(group) &&
    Object.keys(group).length === 1 && Object.hasOwn(group, 'patterns') && Array.isArray(group.patterns) && group.patterns.length === 2 &&
    new Set(group.patterns).size === 2 &&
    group.patterns.every(pattern => ['github/codeql-action', 'github/codeql-action/*'].includes(pattern));
}

function supportedBranchName(name) {
  return name && typeof name === 'object' && !Array.isArray(name) &&
    Object.keys(name).length === 1 && Object.hasOwn(name, 'separator') && name.separator === '/';
}

function supportedIgnores(ignore) {
  // Preserve every dependency's patch/minor coverage, including exact names.
  return Array.isArray(ignore) && ignore.every(rule => rule && typeof rule === 'object' && !Array.isArray(rule) &&
    typeof rule['dependency-name'] === 'string' && Boolean(rule['dependency-name'].trim()) &&
    Object.keys(rule).every(key => ['dependency-name', 'update-types', 'versions'].includes(key)) &&
    (!Object.hasOwn(rule, 'versions') || (Array.isArray(rule.versions) && rule.versions.length === 0)) &&
    Array.isArray(rule['update-types']) && rule['update-types'].length > 0 &&
    rule['update-types'].every(type => type === 'version-update:semver-major'));
}

function inspectUpdates(files, failures) {
  // Intentional compatibility limits: five independent scopes and only the options validated below.
  // New scopes/options require policy review; this is not the full Dependabot schema.
  const scopes = [['gomod', '/'], ['github-actions', '/'], ['npm', '/.github/aislop'],
    ['npm', '/.github/scripts'], ['npm', '/clients/gnome']];
  const keys = ['package-ecosystem', 'directory', 'schedule', 'cooldown', 'rebase-strategy',
    'open-pull-requests-limit', 'groups', 'ignore', 'pull-request-branch-name', 'versioning-strategy'];
  try {
    const config = YAML.parse(files['.github/dependabot.yml'], { uniqueKeys: true });
    if (config?.version !== 2 || Object.keys(config).some(key => !['version', 'updates'].includes(key)) ||
        !Array.isArray(config.updates) || config.updates.length !== scopes.length ||
        !scopes.every(([ecosystem, directory]) =>
          config.updates.some(entry => entry?.['package-ecosystem'] === ecosystem &&
            entry.directory === directory && Object.keys(entry).every(key => keys.includes(key)) &&
            (!Object.hasOwn(entry, 'cooldown') || supportedCooldown(entry.cooldown)) &&
            (!Object.hasOwn(entry, 'groups') || supportedGroups(entry.groups, ecosystem)) &&
            (!Object.hasOwn(entry, 'rebase-strategy') || entry['rebase-strategy'] === 'auto') &&
            (!Object.hasOwn(entry, 'versioning-strategy') || (ecosystem === 'npm' &&
              ['auto', 'increase', 'increase-if-necessary', 'widen'].includes(entry['versioning-strategy']))) &&
            (!Object.hasOwn(entry, 'open-pull-requests-limit') ||
              (Number.isInteger(entry['open-pull-requests-limit']) && entry['open-pull-requests-limit'] > 0)) &&
            (!Object.hasOwn(entry, 'pull-request-branch-name') || supportedBranchName(entry['pull-request-branch-name'])) &&
            (!Object.hasOwn(entry, 'ignore') || supportedIgnores(entry.ignore)) &&
            supportedSchedule(entry.schedule)))) {
      failures.push('Dependabot lost required update coverage');
    }
  } catch (error) {
    failures.push(`Dependabot config is invalid: ${error.message}`);
  }
}

function inspectSource(files, failures) {
  const source = compact(files[DEPENDABOT_SOURCES[0]]);
  for (const contract of [
    "const BOT = { login: 'dependabot[bot]', id: 49699333 };",
    'const MINIMUM_SCORE = 75;',
    "const UPDATE_TYPES = new Set(['version-update:semver-patch', 'version-update:semver-minor']);",
    "return isDependabot(pull.user) && pull.state === 'open' && !pull.draft && pull.head.repo?.full_name === repository && pull.head.ref.startsWith('dependabot/') && pull.base.ref === branch;",
    "'INPUT_COMPAT-LOOKUP': 'true',",
    "'INPUT_SKIP-VERIFICATION': 'false',",
    "'INPUT_SKIP-COMMIT-VERIFICATION': 'false',",
    'return user?.login === BOT.login && user?.id === BOT.id;',
    'if (!commits.length || !commits.every(c => isDependabot(c.author) && c.commit.verification?.verified)) return false;',
    'return Array.isArray(dependencies) && dependencies.length > 0 && dependencies.every(d => UPDATE_TYPES.has(d.updateType) && d.prevVersion && d.newVersion && Number.isInteger(d.compatScore) && d.compatScore >= MINIMUM_SCORE && d.compatScore <= 100);',
    "if (pull.auto_merge?.enabled_by?.login === 'github-actions[bot]') { merge(repository, pull.number, ['--disable-auto']); }",
    'const dependencies = metadata(pull, repository, metadataPath);',
    'if (!eligible(commits, dependencies)) {',
    'if (!trustedPull(current, repository, branch) || current.head.sha !== pull.head.sha) continue;',
    "if (!current.auto_merge) merge(repository, pull.number, ['--auto', '--squash', '--match-head-commit', pull.head.sha]);",
    'await ensureMainRun({ github, context, core, branch, mainWorkflow });',
    'await github.rest.actions.createWorkflowDispatch({ ...repo, workflow_id: mainWorkflow, ref: branch });',
  ]) {
    if (!source.includes(compact(contract))) failures.push('Dependabot source lost safety contract: ' + contract);
  }
}

function inspectDependabot(files, workflows, failures, checks) {
  inspectWorkflow(workflows, failures, checks);
  inspectRecovery(workflows, failures, checks);
  inspectUpdates(files, failures);
  inspectSource(files, failures);
}

async function verifyDependabotAuthor(github, context, reviewed = false) {
  const params = { ...context.repo, pull_number: context.payload.pull_request.number };
  const pull = context.payload.pull_request;
  if (![pull.base?.sha, pull.head?.sha].every(ref => typeof ref === 'string' && /^[a-f0-9]{40}$/.test(ref))) {
    throw new Error('Immutable PR source refs required');
  }
  async function identity(path, ref) {
    try {
      const { data } = await github.rest.repos.getContent({ ...context.repo, path, ref });
      if (data.type !== 'file' || typeof data.sha !== 'string' || !/^[a-f0-9]{40}$/.test(data.sha)) {
        throw new Error(`Cannot identify protected source: ${path}`);
      }
      return data.sha;
    } catch (error) {
      if (error.status === 404) return null;
      throw error;
    }
  }
  let changed = false;
  for (const path of DEPENDABOT_SOURCES) {
    // Compare immutable event refs, never the PR's mutable current file list.
    const before = await identity(path, pull.base.sha);
    const after = await identity(path, pull.head.sha);
    changed ||= before !== after;
  }
  if (!changed || reviewed) return;
  const { data: current } = await github.rest.pulls.get(params);
  if (!['OWNER', 'MEMBER', 'COLLABORATOR'].includes(current.author_association) ||
      current.user?.type !== 'User' || current.user?.login?.endsWith('[bot]')) {
    throw new Error('Dependabot automation source changes require a trusted human author');
  }
}

module.exports = { inspectDependabot, verifyDependabotAuthor };
