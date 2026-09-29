const { mkdtempSync, writeFileSync, readFileSync, rmSync } = require('node:fs');
const { tmpdir } = require('node:os');
const { join } = require('node:path');
const { execFileSync } = require('node:child_process');

const BOT = { login: 'dependabot[bot]', id: 49699333 };
const MINIMUM_SCORE = 75;
const UPDATE_TYPES = new Set(['version-update:semver-patch', 'version-update:semver-minor']);

function isDependabot(user) {
  return user?.login === BOT.login && user?.id === BOT.id;
}

function trustedPull(pull, repository, branch) {
  return isDependabot(pull.user) && pull.state === 'open' && !pull.draft &&
    pull.head.repo?.full_name === repository && pull.head.ref.startsWith('dependabot/') &&
    pull.base.ref === branch;
}

function eligible(commits, dependencies) {
  if (!commits.length || !commits.every(c => isDependabot(c.author) &&
      c.commit.verification?.verified)) return false;
  return Array.isArray(dependencies) && dependencies.length > 0 &&
    dependencies.every(d => UPDATE_TYPES.has(d.updateType) && d.prevVersion && d.newVersion &&
      Number.isInteger(d.compatScore) && d.compatScore >= MINIMUM_SCORE && d.compatScore <= 100);
}

function readDependencies(output) {
  const lines = output.split(/\r?\n/);
  const marker = 'updated-dependencies-json<<';
  const start = lines.findIndex(line => line.startsWith(marker));
  if (start < 0) throw new Error('Dependabot metadata is missing');
  const delimiter = lines[start].slice(marker.length);
  const end = lines.indexOf(delimiter, start + 1);
  if (!delimiter || end < 0) throw new Error('Incomplete Dependabot metadata');
  return JSON.parse(lines.slice(start + 1, end).join('\n'));
}

function fetchMetadata(pull, repository, metadataPath) {
  const directory = mkdtempSync(join(tmpdir(), 'dependabot-metadata-'));
  try {
    const event = join(directory, 'event.json');
    const output = join(directory, 'output');
    writeFileSync(event, JSON.stringify({ pull_request: pull }));
    writeFileSync(output, '');
    execFileSync(process.execPath, [join(metadataPath, 'dist/index.js')], {
      timeout: 90000,
      maxBuffer: 4 * 1024 * 1024,
      env: {
        HOME: process.env.HOME,
        PATH: process.env.PATH,
        RUNNER_TEMP: process.env.RUNNER_TEMP,
        NODE_OPTIONS: process.env.NODE_OPTIONS,
        GITHUB_REPOSITORY: repository,
        GITHUB_EVENT_NAME: 'pull_request_target',
        GITHUB_EVENT_PATH: event,
        GITHUB_OUTPUT: output,
        'INPUT_GITHUB-TOKEN': process.env.GH_TOKEN,
        'INPUT_COMPAT-LOOKUP': 'true',
        'INPUT_ALERT-LOOKUP': '',
        'INPUT_SKIP-VERIFICATION': 'false',
        'INPUT_SKIP-COMMIT-VERIFICATION': 'false',
      },
    });
    return readDependencies(readFileSync(output, 'utf8'));
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
}

function mergeCommand(repository, number, options) {
  execFileSync('gh', ['pr', 'merge', String(number), '--repo', repository, ...options],
    { timeout: 30000, stdio: 'pipe' });
}

async function ensureMainRun({ github, context, core, branch, mainWorkflow }) {
  const repo = context.repo;
  const { data: tip } = await github.rest.repos.getBranch({ ...repo, branch });
  const sha = tip.commit.sha;
  const pulls = await github.paginate(github.rest.repos.listPullRequestsAssociatedWithCommit,
    { ...repo, commit_sha: sha, per_page: 100 });
  if (!pulls.some(p => isDependabot(p.user) && p.merged_at &&
      p.merge_commit_sha === sha && p.base.ref === branch)) return;
  const runs = await github.paginate(github.rest.actions.listWorkflowRuns,
    { ...repo, workflow_id: mainWorkflow, branch, head_sha: sha, per_page: 100 });
  if (runs.some(r => r.head_sha === sha && ['push', 'workflow_dispatch'].includes(r.event) &&
      !['cancelled', 'skipped'].includes(r.conclusion))) return;
  await github.rest.actions.createWorkflowDispatch({ ...repo, workflow_id: mainWorkflow, ref: branch });
  core.info(`Requested ${mainWorkflow} for Dependabot merge at ${sha}`);
}

async function run({ github, context, core, metadataPath, mainWorkflow,
  metadata = fetchMetadata, merge = mergeCommand }) {
  const repository = `${context.repo.owner}/${context.repo.repo}`;
  const { data: repo } = await github.rest.repos.get(context.repo);
  const branch = repo.default_branch;
  if (!['pull_request_target', 'workflow_call'].includes(context.eventName) &&
      context.ref !== `refs/heads/${branch}`) throw new Error('Default branch required');
  const number = context.payload.pull_request?.number;
  const pulls = number ? [{ number }] : await github.paginate(github.rest.pulls.list,
    { ...context.repo, state: 'open', base: branch, per_page: 100 });
  let failed = false;
  for (const candidate of pulls) {
    try {
      const params = { ...context.repo, pull_number: candidate.number };
      const { data: pull } = await github.rest.pulls.get(params);
      if (!trustedPull(pull, repository, branch)) continue;
      if (pull.auto_merge?.enabled_by?.login === 'github-actions[bot]') {
        merge(repository, pull.number, ['--disable-auto']);
      }
      const commits = await github.paginate(github.rest.pulls.listCommits, { ...params, per_page: 100 });
      const dependencies = metadata(pull, repository, metadataPath);
      if (!eligible(commits, dependencies)) {
        core.info(`#${pull.number}: manual review required (identity, update type, or compatibility score)`);
        continue;
      }
      const { data: current } = await github.rest.pulls.get(params);
      if (!trustedPull(current, repository, branch) || current.head.sha !== pull.head.sha) continue;
      if (!current.auto_merge) merge(repository, pull.number,
        ['--auto', '--squash', '--match-head-commit', pull.head.sha]);
      core.info(`#${pull.number}: eligible for gated squash auto-merge`);
    } catch (error) {
      failed = true;
      core.warning(`#${candidate.number}: evaluation failed: ${error.message}`);
    }
  }
  await ensureMainRun({ github, context, core, branch, mainWorkflow });
  if (failed) core.setFailed('Dependabot evaluation failed; retry on refresh');
}

module.exports = { run, trustedPull, eligible, readDependencies, ensureMainRun };
