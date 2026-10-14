const YAML = require('yaml');
const { canonical, normalizeActionSteps } = require('./policy-action-pins.cjs');

// Selected accident tripwires, not a proof of arbitrary JavaScript semantics.
// Trusted code execution, human review and behavioral tests remain the boundary.
const DEPENDABOT_SOURCES = [
  '.github/scripts/dependabot-auto-merge.cjs', '.github/scripts/policy-guard.cjs',
  '.github/scripts/policy-dependabot.cjs',
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
  if (!Array.isArray(activities) || !['opened', 'synchronize', 'reopened', 'ready_for_review', 'closed']
    .every(activity => activities.includes(activity))) {
    failures.push('Dependabot lost required PR activities');
  }
  const queue = steps.find(step => step.name === 'Queue eligible verified updates');
  const script = `await require('./automation/.github/scripts/dependabot-auto-merge.cjs').run({
    github, context, core,
    metadataPath: require('node:path').resolve('metadata-action'),
    mainWorkflow: 'main.yml',
  });`;
  if (job?.if !== "github.event_name != 'pull_request_target' || github.event.pull_request.user.login == 'dependabot[bot]'" ||
      workflow?.concurrency?.['cancel-in-progress'] !== false ||
      workflow?.concurrency?.group !== 'dependabot-auto-merge-${{ github.repository }}') {
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
      job?.['runs-on'] !== 'ubuntu-latest' || job?.['timeout-minutes'] !== 15) {
    failures.push('Dependabot lost trusted executable workflow structure');
  }
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
  if (!workflows[refresh]?.on?.schedule?.some(item => typeof item.cron === 'string' && item.cron.trim())) {
    failures.push('Dependabot lost scheduled recovery');
  }
  for (const document of [workflow, workflows[refresh]]) {
    for (const scope of [document, ...Object.values(document?.jobs || {})]) {
      if (scope !== document && !Object.hasOwn(scope, 'permissions')) continue;
      const permissions = scope?.permissions;
      if (!permissions || Object.keys(permissions).length !== 3 ||
          !['contents', 'pull-requests', 'actions'].every(key => permissions[key] === 'write')) {
        failures.push('Dependabot lost required automation permissions');
      }
    }
  }
}

function inspectUpdates(files, failures) {
  try {
    const config = YAML.parse(files['.github/dependabot.yml'], { uniqueKeys: true });
    if (config?.version !== 2 || !Array.isArray(config.updates) ||
        ![['gomod', '/'], ['github-actions', '/'], ['npm', '/.github/aislop'],
          ['npm', '/.github/scripts'], ['npm', '/clients/gnome']].every(([ecosystem, directory]) =>
          config.updates.some(entry => entry?.['package-ecosystem'] === ecosystem &&
            entry.directory === directory && entry['open-pull-requests-limit'] !== 0 &&
            !Object.hasOwn(entry, 'allow') && !Object.hasOwn(entry, 'target-branch') &&
            !Object.hasOwn(entry, 'exclude-paths') &&
            (!Object.hasOwn(entry, 'pull-request-branch-name') ||
              (entry['pull-request-branch-name'] &&
                Object.keys(entry['pull-request-branch-name']).every(key => key === 'separator') &&
                entry['pull-request-branch-name'].separator === '/')) &&
            !entry.ignore?.some(rule => /^\*+$/.test(rule['dependency-name']) &&
              (!Array.isArray(rule['update-types']) || rule['update-types'].some(type =>
                ['version-update:semver-patch', 'version-update:semver-minor'].includes(type)))) &&
            ['daily', 'weekly', 'monthly', 'cron'].includes(entry.schedule?.interval) &&
            (entry.schedule.interval !== 'cron' || entry.schedule.cronjob?.trim())))) {
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

async function verifyDependabotAuthor(github, context) {
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
  if (!changed) return;
  const { data: current } = await github.rest.pulls.get(params);
  if (!['OWNER', 'MEMBER', 'COLLABORATOR'].includes(current.author_association) ||
      current.user?.type !== 'User' || current.user?.login?.endsWith('[bot]')) {
    throw new Error('Dependabot automation source changes require a trusted human author');
  }
}

module.exports = { inspectDependabot, verifyDependabotAuthor };
