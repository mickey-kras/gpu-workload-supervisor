const YAML = require('yaml');

const REQUIRED_FILES = [
  '.github/workflows/pr-validation.yml', '.github/workflows/ci.yml',
  '.github/workflows/main.yml', '.github/workflows/policy-guard.yml',
  '.github/workflows/release.yml', '.github/workflows/codeql.yml',
  '.github/workflows/aislop.yml', '.github/workflows/dependency-review.yml',
  '.github/workflows/branch-policy.yml', '.github/workflows/dependabot-auto-merge.yml',
  '.github/workflows/dependabot-auto-merge-refresh.yml', '.github/dependabot.yml',
  '.github/scripts/policy-guard.cjs', '.github/scripts/dependabot-auto-merge.cjs',
  '.github/scripts/pr-branch-updater.cjs', 'sonar-project.properties',
  '.github/scripts/package.json', '.github/scripts/package-lock.json',
  '.github/aislop/package.json', '.github/aislop/package-lock.json',
  '.github/dependency-review-config.yml', '.semgrep.yml', '.aislop/config.yml',
  '.github/rulesets/enforce-release-tag-names.json',
  '.github/rulesets/enforce-work-branch-names.json',
  '.github/rulesets/protect-default-branch.json',
  '.github/rulesets/protect-release-branches.json',
  '.github/rulesets/protect-release-tags.json',
  '.github/rulesets/release-branch-creation.json',
  '.github/rulesets/release-branch-deletion.json',
  '.github/rulesets/release-tag-creation.json',
];

function inspect(files) {
  const failures = [];
  const workflows = {};
  for (const path of REQUIRED_FILES) {
    if (typeof files[path] !== 'string') failures.push(`Missing required file: ${path}`);
  }
  for (const [path, content] of Object.entries(files)) {
    if (!path.startsWith('.github/workflows/') || !/\.ya?ml$/.test(path)) continue;
    try {
      const doc = YAML.parseDocument(content, { uniqueKeys: true });
      if (doc.errors.length) throw doc.errors[0];
      workflows[path] = doc.toJS();
      if (workflows[path]?.defaults?.run?.shell && workflows[path].defaults.run.shell !== 'bash') {
        failures.push(`${path} overrides the workflow shell`);
      }
      const jobs = workflows[path]?.jobs || {};
      for (const [jobId, job] of Object.entries(jobs)) {
        if (job.defaults?.run?.shell && job.defaults.run.shell !== 'bash') {
          failures.push(`${path}/${jobId} overrides the job shell`);
        }
        if (Object.hasOwn(job, 'continue-on-error')) {
          failures.push(`${path}/${jobId} may ignore gate failures`);
        }
        for (const step of [job, ...(job.steps || [])]) {
          if (Object.hasOwn(step, 'continue-on-error')) {
            failures.push(`${path}/${jobId} may ignore step failures`);
          }
          if (!step.uses || step.uses.startsWith('./')) continue;
          if (!/^[\w.-]+(?:\/[\w.-]+)+@[a-f0-9]{40}$/.test(step.uses)) {
            failures.push(`${path}/${jobId} has an unpinned action: ${step.uses}`);
          }
        }
      }
    } catch (error) {
      failures.push(`${path} has invalid YAML: ${error.message}`);
    }
  }

  function event(path, name) {
    if (!Object.hasOwn(workflows[path]?.on || {}, name)) failures.push(`${path} lost event ${name}`);
  }
  function job(path, id, uses) {
    const found = workflows[path]?.jobs?.[id];
    if (!found || Object.hasOwn(found, 'if') || found.uses !== uses) {
      failures.push(`${path} lost unconditional job ${id}: ${uses}`);
    }
  }
  function step(path, jobId, name, { uses, run, withValues, expectedIf, allowJobIf = false } = {}) {
    const job = workflows[path]?.jobs?.[jobId];
    const found = job?.steps?.find(s => s.name === name);
    const commands = typeof found?.run === 'string' ? found.run.split('\n').map(line => line.trim())
      .filter(line => line && !line.startsWith('#')) : [];
    if (!found || (!allowJobIf && Object.hasOwn(job, 'if')) ||
        (expectedIf === undefined ? Object.hasOwn(found, 'if') : found.if !== expectedIf) ||
        (found.shell && found.shell !== 'bash') || (uses && !found.uses?.startsWith(`${uses}@`)) ||
        (run && !run.every(part => commands.some(line => line === part || line.startsWith(`${part} `)))) ||
        (withValues && !Object.entries(withValues).every(([key, value]) => found.with?.[key] === value))) {
      failures.push(`${path}/${jobId} lost unconditional control: ${name}`);
    }
  }
  function exactRun(path, jobId, name, lines) {
    const actual = workflows[path]?.jobs?.[jobId]?.steps?.find(s => s.name === name)?.run;
    const normalized = value => typeof value === 'string' ? value.trim().split('\n')
      .map(line => line.trim()).join('\n') : null;
    if (normalized(actual) !== normalized(lines.join('\n'))) {
      failures.push(`${path}/${jobId} changed gate commands: ${name}`);
    }
  }
  const pr = '.github/workflows/pr-validation.yml';
  event(pr, 'pull_request');
  for (const [id, target] of Object.entries({
    quality: 'ci', aislop: 'aislop', codeql: 'codeql',
    'branch-policy': 'branch-policy', 'dependency-review': 'dependency-review',
  })) job(pr, id, `./.github/workflows/${target}.yml`);

  const ci = '.github/workflows/ci.yml';
  event(ci, 'workflow_call');
  step(ci, 'checks', 'Go formatting', { run: ['test -z "$(gofmt -l .)"'] });
  step(ci, 'checks', 'Go module lock is current', { run: ['go mod tidy', 'git diff --exit-code -- go.mod go.sum'] });
  step(ci, 'checks', 'Tests, race detector, and coverage', { run: ['go test -race -coverprofile=coverage.out ./...', 'awk'] });
  step(ci, 'checks', 'Go vet', { run: ['go vet ./...'] });
  step(ci, 'checks', 'Go vulnerability audit', { uses: 'golang/govulncheck-action' });
  step(ci, 'checks', 'Audit Aislop toolchain', { run: ['npm audit --prefix .github/aislop --audit-level=moderate'] });
  step(ci, 'checks', 'Test policy automation', { run: ['node --test .github/scripts/*.test.cjs'] });
  step(ci, 'checks', 'Gitleaks', { uses: 'gitleaks/gitleaks-action' });
  step(ci, 'checks', 'Semgrep', { run: ['docker pull "$SEMGREP_IMAGE"', 'semgrep scan --config .semgrep.yml --exclude .semgrep.yml --error'] });
  step(ci, 'checks', 'Trivy high and critical gate', { uses: 'aquasecurity/trivy-action', withValues: { 'exit-code': '1', severity: 'HIGH,CRITICAL' } });
  step(ci, 'checks', 'Upload Trivy SARIF', {
    uses: 'github/codeql-action/upload-sarif',
    expectedIf: "${{ !cancelled() && (github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository) }}",
  });
  exactRun(ci, 'checks', 'Go formatting', ['test -z "$(gofmt -l .)"']);
  exactRun(ci, 'checks', 'Go module lock is current', [
    'go mod tidy', 'git diff --exit-code -- go.mod go.sum',
  ]);
  exactRun(ci, 'checks', 'Tests, race detector, and coverage', [
    'go test -race -coverprofile=coverage.out ./...',
    'go tool cover -func=coverage.out | tee coverage-summary.txt',
    'awk \'$1 == "total:" { coverage=$3+0; found=1 } END { if (!found || coverage < 53) exit 1 }\' coverage-summary.txt',
  ]);
  exactRun(ci, 'checks', 'Go vet', ['go vet ./...']);
  exactRun(ci, 'checks', 'Audit Aislop toolchain', [
    'npm ci --prefix .github/aislop --ignore-scripts --no-audit --no-fund',
    'npm audit --prefix .github/aislop --audit-level=moderate',
  ]);
  exactRun(ci, 'checks', 'Test policy automation', [
    'npm ci --prefix .github/scripts --ignore-scripts --no-audit --no-fund',
    'npm audit --prefix .github/scripts --audit-level=moderate',
    'node --test .github/scripts/*.test.cjs',
  ]);
  exactRun(ci, 'checks', 'Semgrep', [
    'SEMGREP_IMAGE="semgrep/semgrep:1.172.0@sha256:65dcd4408adda7c183a6b4550cb1e9b19f7f627a6fbb7e0559bd466bedc44d7b"',
    'docker pull "$SEMGREP_IMAGE"',
    'docker run --rm -v "${PWD}:/src" -w /src "$SEMGREP_IMAGE" \\',
    'semgrep scan --config .semgrep.yml --exclude .semgrep.yml --error',
  ]);

  const main = '.github/workflows/main.yml';
  event(main, 'push');
  for (const id of ['quality', 'aislop', 'codeql']) job(main, id, `./.github/workflows/${id === 'quality' ? 'ci' : id}.yml`);
  step(main, 'sonar', 'Generate Go coverage', { run: ['go test -coverprofile=coverage.out ./...'] });
  step(main, 'sonar', 'SonarQube analysis', { uses: 'SonarSource/sonarqube-scan-action' });
  step(main, 'sonar', 'SonarQube quality gate', { uses: 'SonarSource/sonarqube-quality-gate-action' });
  if (!workflows[main]?.jobs?.sonar?.steps?.some(s => s.uses?.startsWith('tailscale/github-action@'))) {
    failures.push(`${main} lost Tailscale connectivity`);
  }

  const guard = '.github/workflows/policy-guard.yml';
  event(guard, 'pull_request_target');
  step(guard, 'guard', 'Install trusted policy dependencies', { run: ['npm ci --prefix .github/scripts'] });
  step(guard, 'guard', 'Verify policy at PR head using trusted code', { uses: 'actions/github-script' });
  const trustedGuard = workflows[guard]?.jobs?.guard?.steps?.find(s => s.uses?.startsWith('actions/checkout@'));
  if (trustedGuard?.with?.ref !== '${{ github.workflow_sha }}' || trustedGuard.with['persist-credentials'] !== false) {
    failures.push(`${guard} lost trusted checkout`);
  }

  const dependency = '.github/workflows/dependency-review.yml';
  event(dependency, 'workflow_call');
  step(dependency, 'review', 'Dependency review', { uses: 'actions/dependency-review-action' });
  const release = '.github/workflows/release.yml';
  event(release, 'workflow_dispatch');
  step(release, 'publish', 'Release App token', { uses: 'actions/create-github-app-token' });
  step(release, 'publish', 'Publish immutable GitHub release', {
    run: ['gh release create'], expectedIf: "steps.state.outputs.published != 'true'",
  });
  const bot = '.github/workflows/dependabot-auto-merge.yml';
  event(bot, 'pull_request_target');
  step(bot, 'enable-auto-merge', 'Queue eligible verified updates', { uses: 'actions/github-script', allowJobIf: true });
  if (workflows[bot]?.jobs?.['enable-auto-merge']?.if !==
      "github.event_name != 'pull_request_target' || github.event.pull_request.user.login == 'dependabot[bot]'") {
    failures.push(`${bot} lost Dependabot-only execution condition`);
  }
  const trustedBot = workflows[bot]?.jobs?.['enable-auto-merge']?.steps?.find(s => s.name === 'Read trusted automation');
  if (trustedBot?.with?.ref !== '${{ github.workflow_sha }}') failures.push(`${bot} lost trusted checkout`);
  for (const [path, jobId, name, expected] of [
    ['.github/workflows/codeql.yml', 'analyze', undefined, 'github/codeql-action/analyze'],
    ['.github/workflows/aislop.yml', 'status', 'Aislop Go quality gate', undefined],
  ]) {
    const steps = workflows[path]?.jobs?.[jobId]?.steps || [];
    if (!steps.some(s => !Object.hasOwn(s, 'if') && (name ? s.name === name && s.run?.includes('aislop ci --human internal') : s.uses?.startsWith(`${expected}@`)))) {
      failures.push(`${path} lost required ${name || expected}`);
    }
  }
  if (!files['sonar-project.properties']?.includes('sonar.go.coverage.reportPaths=coverage.out')) {
    failures.push('SonarQube lost Go coverage path');
  }
  const rulesets = {};
  for (const path of REQUIRED_FILES.filter(name => name.startsWith('.github/rulesets/'))) {
    try {
      rulesets[path.split('/').pop()] = JSON.parse(files[path]);
      if (rulesets[path.split('/').pop()].enforcement !== 'active') {
        failures.push(`${path} lost active enforcement`);
      }
    } catch (error) {
      if (files[path]) failures.push(`${path} has invalid JSON: ${error.message}`);
    }
  }
  const defaultRules = rulesets['protect-default-branch.json']?.rules || [];
  const statusRule = defaultRules.find(rule => rule.type === 'required_status_checks');
  const checks = statusRule?.parameters?.required_status_checks || [];
  for (const context of ['quality / checks', 'aislop / aislop status', 'codeql / analyze',
    'branch-policy / branch name', 'dependency-review / dependency review', 'guard']) {
    if (!checks.some(check => check.context === context && check.integration_id === 15368)) {
      failures.push(`Default branch lost required check: ${context}`);
    }
  }
  if (statusRule?.parameters?.strict_required_status_checks_policy !== true ||
      defaultRules.find(rule => rule.type === 'pull_request')?.parameters
        ?.required_review_thread_resolution !== true) {
    failures.push('Default branch lost strict checks or review-thread resolution');
  }
  for (const type of ['pull_request', 'code_scanning', 'code_quality', 'deletion', 'non_fast_forward']) {
    if (!defaultRules.some(rule => rule.type === type)) failures.push(`Default branch lost rule: ${type}`);
  }
  const tagRules = rulesets['protect-release-tags.json']?.rules || [];
  for (const type of ['update', 'deletion', 'non_fast_forward']) {
    if (!tagRules.some(rule => rule.type === type)) failures.push(`Release tags lost rule: ${type}`);
  }
  try {
    const config = YAML.parse(files['.github/dependency-review-config.yml']);
    if (config['fail-on-severity'] !== 'high' ||
        !['AGPL-3.0-only', 'GPL-3.0-only', 'SSPL-1.0'].every(license =>
          config['deny-licenses']?.includes(license)) ||
        config['allow-dependencies-licenses']?.length !== 0) {
      failures.push('Dependency review policy was weakened');
    }
  } catch (error) {
    if (files['.github/dependency-review-config.yml']) failures.push(`Dependency review config is invalid: ${error.message}`);
  }
  try {
    const config = YAML.parse(files['.aislop/config.yml']);
    if (typeof config.ci?.failBelow !== 'number' || config.ci.failBelow < 95 ||
        config.rules?.['security/hardcoded-secret'] !== 'error') {
      failures.push('Aislop policy was weakened');
    }
  } catch (error) {
    if (files['.aislop/config.yml']) failures.push(`Aislop config is invalid: ${error.message}`);
  }
  try {
    const rules = YAML.parse(files['.semgrep.yml']).rules;
    const shell = rules.find(rule => rule.id === 'go.exec-shell');
    const tls = rules.find(rule => rule.id === 'go.tls-insecure');
    const injection = rules.find(rule => rule.id === 'github-actions.shell-injection');
    if (shell?.severity !== 'ERROR' || shell.pattern !== 'exec.Command("sh", "-c", ...)' ||
        tls?.severity !== 'ERROR' || tls.pattern !== 'tls.Config{..., InsecureSkipVerify: true, ...}' ||
        injection?.severity !== 'ERROR' || injection.patterns?.length !== 3 ||
        injection.patterns[1]?.pattern?.trim() !== 'run: $RUN' ||
        !injection.patterns[2]?.['metavariable-regex']?.regex?.includes('github\\.event\\.')) {
      failures.push('Semgrep policy was weakened');
    }
  } catch (error) {
    if (files['.semgrep.yml']) failures.push(`Semgrep config is invalid: ${error.message}`);
  }
  return failures;
}

async function run({ github, context }) {
  const { owner, repo } = context.repo;
  const sha = context.payload.pull_request.head.sha;
  const { data: commit } = await github.rest.git.getCommit({ owner, repo, commit_sha: sha });
  const { data: tree } = await github.rest.git.getTree({
    owner, repo, tree_sha: commit.tree.sha, recursive: 'true',
  });
  if (tree.truncated) throw new Error('Cannot verify a truncated PR tree');
  const paths = tree.tree.filter(entry => entry.type === 'blob' &&
    (REQUIRED_FILES.includes(entry.path) ||
      (entry.path.startsWith('.github/workflows/') && /\.ya?ml$/.test(entry.path))))
    .map(entry => entry.path);
  const files = {};
  for (const path of paths) {
    const { data } = await github.rest.repos.getContent({ owner, repo, path, ref: sha });
    if (!('content' in data)) throw new Error(`Unexpected directory: ${path}`);
    files[path] = Buffer.from(data.content, data.encoding || 'base64').toString('utf8');
  }
  const failures = inspect(files);
  if (failures.length) throw new Error(failures.join('\n'));
}

module.exports = { inspect, run };
