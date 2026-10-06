const YAML = require('yaml');
const { createHash } = require('node:crypto');
const { inspectReleaseEntry, inspectReleasePublish } = require('./policy-release.cjs');

const REQUIRED_FILES = [
  '.github/workflows/pr-validation.yml', '.github/workflows/ci.yml',
  '.github/workflows/main.yml', '.github/workflows/policy-guard.yml',
  '.github/workflows/release.yml', '.github/workflows/codeql.yml',
  '.github/workflows/aislop.yml', '.github/workflows/dependency-review.yml',
  '.github/workflows/branch-policy.yml', '.github/workflows/dependabot-auto-merge.yml',
  '.github/workflows/dependabot-auto-merge-refresh.yml', '.github/dependabot.yml',
  '.github/scripts/policy-guard.cjs', '.github/scripts/dependabot-auto-merge.cjs',
  '.github/scripts/release-settings.cjs', '.github/scripts/release-follow-up.cjs',
  'release-version.json', '.github/scripts/policy-release.cjs',
  '.github/scripts/pr-branch-updater.cjs', 'sonar-project.properties',
  '.github/scripts/package.json', '.github/scripts/package-lock.json',
  '.github/aislop/package.json', '.github/aislop/package-lock.json',
  '.github/dependency-review-config.yml', '.semgrep.yml', '.aislop/config.yml',
  '.goreleaser.yaml', '.testcoverage.yml',
  '.github/actions/setup-goreleaser/action.yml', '.github/scripts/install-goreleaser.sh',
];

function scanWorkflows(files, failures) {
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

  return workflows;
}

function createChecks(workflows, failures) {
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
  return { event, job, step, exactRun };
}

function inspectGoReleaser({ files, workflows, failures, checks, path, jobId, builds, expectedIf }) {
  const steps = workflows[path]?.jobs?.[jobId]?.steps || [];
  const setupName = 'Install verified GoReleaser';
  checks.step(path, jobId, setupName, { expectedIf });
  const setupIndex = steps.findIndex(s => s.name === setupName);
  if (steps[setupIndex]?.uses !== './.github/actions/setup-goreleaser' ||
      builds.some(([name]) => steps.findIndex(s => s.name === name) <= setupIndex)) {
    failures.push(`${path}/${jobId} lost verified GoReleaser setup ordering`);
  }
  for (const [name, args] of builds) {
    checks.step(path, jobId, name, { expectedIf });
    checks.exactRun(path, jobId, name, [`goreleaser ${args}`]);
  }
  for (const [file, digest] of [
    ['.github/actions/setup-goreleaser/action.yml', '5265ee0469a1f52be3905b173e81ba6e70aa15c13d57f1986b1aec7cfdd11b9a'],
    ['.github/scripts/install-goreleaser.sh', 'de2e876e51298aec0ec86a42fdb850aaa2f95e3d86cd1fa67a6d5c1febbcc8b0'],
  ]) {
    if (createHash('sha256').update(files[file] || '').digest('hex') !== digest) {
      failures.push(`${file} changed verified GoReleaser installer`);
    }
  }
}

function inspectTrivyUpload(steps, failures) {
  const trusted = "(github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository)";
  const guarded = "${{ !cancelled() && steps.trivy_report.outcome == 'success' && " + trusted + " }}";
  const uploadIndex = steps.findIndex(step => step.name === 'Upload Trivy SARIF');
  const upload = steps[uploadIndex];
  const reportIndex = steps.findIndex(step => step.name === 'Trivy filesystem report');
  const report = steps[reportIndex];
  if (!report || reportIndex >= uploadIndex || report.id !== 'trivy_report' ||
      steps.filter(step => step.id === 'trivy_report').length !== 1 ||
      Object.hasOwn(report, 'if') || Object.hasOwn(report, 'continue-on-error') ||
      !report.uses?.startsWith('aquasecurity/trivy-action@') ||
      !Object.entries({
        'scan-type': 'fs', 'scan-ref': '.', format: 'sarif',
        output: 'trivy-results.sarif', severity: 'HIGH,CRITICAL',
        'ignore-unfixed': true, 'exit-code': '0',
      }).every(([key, value]) => report.with?.[key] === value) ||
      upload.with?.sarif_file !== 'trivy-results.sarif' ||
      upload.with?.category !== '.github/workflows/ci.yml:fs') {
    failures.push('Trivy SARIF upload lost its required report producer');
  }
  return guarded;
}

function inspectCi(files, workflows, failures, checks) {
  const { event, job, step, exactRun } = checks;
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
  step(ci, 'checks', 'Package coverage floors');
  exactRun(ci, 'checks', 'Package coverage floors', [
    'go run github.com/vladopajic/go-test-coverage/v2@v2.19.0 --config=.testcoverage.yml',
  ]);
  step(ci, 'checks', 'Go vet', { run: ['go vet ./...'] });
  inspectGoReleaser({ files, workflows, failures, checks, path: ci, jobId: 'checks', builds: [
    ['Validate GoReleaser configuration', 'check'], ['Build snapshot artifacts', 'release --snapshot --clean'],
  ] });
  step(ci, 'checks', 'Verify snapshot archives', { run: ['tar -tzf', '(cd dist && sha256sum --check checksums.txt)'] });
  step(ci, 'checks', 'Native desktop transport integration', { run: ['sudo --preserve-env=GITHUB_ACTIONS,RUNNER_ENVIRONMENT timeout 180s bash scripts/native-desktop-integration.sh --isolated-test-host'] });
  step(ci, 'checks', 'Packaged setup lifecycle integration', { run: ['sudo --preserve-env=GITHUB_ACTIONS,RUNNER_ENVIRONMENT timeout 300s bash scripts/setup-desktop-integration.sh --isolated-test-host "${packages[0]}"'] });
  step(ci, 'checks', 'Debian payload lifecycle integration', { run: ['sudo timeout 120s bash scripts/check-desktop-deb-lifecycle.sh "${packages[0]}"'] });
  step(ci, 'checks', 'Go vulnerability audit', { uses: 'golang/govulncheck-action', withValues: { cache: false } });
  step(ci, 'checks', 'Audit Aislop toolchain');
  step(ci, 'checks', 'Test policy automation', { run: ['node --test .github/scripts/*.test.cjs'] });
  step(ci, 'checks', 'Gitleaks', { uses: 'gitleaks/gitleaks-action' });
  step(ci, 'checks', 'Semgrep', { run: ['docker pull "$SEMGREP_IMAGE"', 'semgrep scan --config .semgrep.yml --exclude .semgrep.yml --error'] });
  step(ci, 'checks', 'Trivy high and critical gate', { uses: 'aquasecurity/trivy-action', withValues: { 'exit-code': '1', severity: 'HIGH,CRITICAL' } });
  const uploadCondition = inspectTrivyUpload(workflows[ci]?.jobs?.checks?.steps || [], failures);
  step(ci, 'checks', 'Upload Trivy SARIF', {
    uses: 'github/codeql-action/upload-sarif',
    expectedIf: uploadCondition,
  });
  exactRun(ci, 'checks', 'Go formatting', ['test -z "$(gofmt -l .)"']);
  exactRun(ci, 'checks', 'Go module lock is current', [
    'go mod tidy', 'git diff --exit-code -- go.mod go.sum',
  ]);
  exactRun(ci, 'checks', 'Tests, race detector, and coverage', [
    'go test -race -coverprofile=coverage.out ./...',
    'go tool cover -func=coverage.out | tee coverage-summary.txt',
    'awk \'$1 == "total:" { coverage=$3+0; found=1 } END { if (!found || coverage < 90) exit 1 }\' coverage-summary.txt',
  ]);
  exactRun(ci, 'checks', 'Go vet', ['go vet ./...']);
  const auditRun = workflows[ci]?.jobs?.checks?.steps?.find(s => s.name === 'Audit Aislop toolchain')?.run || '';
  const temporaryAudit = auditRun.includes('.github/audit-tool/');
  if (temporaryAudit) {
    for (const [path, digest] of [['.github/audit-tool/audit-ci.json', '5d31c2834a2cd56fa7c8e6a62ea015c8c07d4da5bacd1e4af779950a3842f9ec'], ['.github/audit-tool/audit-fixture.json', 'b338f05c92807ac45b2c7d50eb8f1dbe6c8c29f671ceb2912c051e39c453ecd2'], ['.github/audit-tool/audit.cjs', '3302195e687c5a4940c88d32353f68bcb12b61475affc05af18cba6398cbbc31'], ['.github/audit-tool/audit.test.cjs', '4271a0e0cabfb6e07c779fa6a15b86751ed55ed25322abdd546e56772a58956b'], ['.github/audit-tool/package-lock.json', 'ac23769398329eb7cea03236037626c2197d20695c35c6f8af26ea7706b73660'], ['.github/audit-tool/package.json', '10e831899e68131e3fa58f3c62c18aeba8fceb87199de65f9861f015b63fb709']]) {
      if (createHash('sha256').update(files[path] || '').digest('hex') !== digest) {
        failures.push(`${path} changed the approved temporary audit exception`);
      }
    }
  }
  exactRun(ci, 'checks', 'Audit Aislop toolchain', temporaryAudit ? [
    'npm ci --prefix .github/audit-tool --ignore-scripts --no-audit --no-fund',
    'npm audit --prefix .github/audit-tool --audit-level=moderate',
    'npm ci --prefix .github/aislop --ignore-scripts --no-audit --no-fund',
    'node --test .github/audit-tool/audit.test.cjs',
    'node .github/audit-tool/audit.cjs .github/aislop',
  ] : [
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

}

function inspectAdditionalWorkflows(files, workflows, failures, checks) {
  const { event, job, step } = checks;
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
  inspectReleaseEntry(files, workflows, failures, checks);
  inspectReleasePublish(files, workflows, failures, checks, inspectGoReleaser);
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
  try {
    const releaseConfig = YAML.parse(files['.goreleaser.yaml']);
    const targets = releaseConfig.builds || [];
    if (!['./cmd/gpu-mode', './cmd/gpu-workload-proxy'].every(target => targets.some(build =>
      build.main === target && build.goos?.includes('linux') &&
      build.goarch?.includes('amd64') && build.goarch?.includes('arm64'))) ||
      releaseConfig.archives?.[0]?.name_template !== '{{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{ .Arch }}' ||
      releaseConfig.checksum?.name_template !== 'checksums.txt') {
      failures.push('GoReleaser lost deployable Linux binaries');
    }
  } catch (error) {
    if (files['.goreleaser.yaml']) failures.push(`GoReleaser config is invalid: ${error.message}`);
  }
}

function inspectScannerConfigs(files, failures) {
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
    if (typeof config.ci?.failBelow !== 'number' || config.ci.failBelow < 100 ||
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
}

function inspectCoverageConfig(files, failures) {
  try {
    const doc = YAML.parseDocument(files['.testcoverage.yml'], { uniqueKeys: true });
    if (doc.errors.length) throw doc.errors[0];
    const config = doc.toJS();
    const floors = new Map([
      ['^internal/control$', 78], ['^internal/store$', 85],
      ['^cmd/gpu-mode$', 93], ['^cmd/gpu-workload-proxy$', 90],
      ['^internal/lock$', 94],
      ['^internal/proxy$', 97], ['^internal/runtime$', 95],
      ['^internal/supervisor$', 93],
    ]);
    const validFloor = (value, minimum) => typeof value === 'number' &&
      Number.isInteger(value) && value >= minimum && value <= 100;
    if (Object.keys(config).some(key => !['profile', 'threshold', 'override'].includes(key)) ||
        config.profile !== 'coverage.out' ||
        !validFloor(config.threshold?.total, 90) || !validFloor(config.threshold?.package, 90) ||
        !Array.isArray(config.override) || config.override.length !== floors.size ||
        new Set(config.override.map(rule => rule.path)).size !== floors.size ||
        config.override.some(rule => !floors.has(rule.path) ||
          !validFloor(rule.threshold, floors.get(rule.path)))) {
      failures.push('Package coverage policy was weakened');
    }
  } catch (error) {
    failures.push(`Package coverage config is invalid: ${error.message}`);
  }
}

function inspect(files) {
  const failures = [];
  const workflows = scanWorkflows(files, failures);
  const checks = createChecks(workflows, failures);
  inspectCi(files, workflows, failures, checks);
  inspectAdditionalWorkflows(files, workflows, failures, checks);
  inspectScannerConfigs(files, failures);
  inspectCoverageConfig(files, failures);
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
      entry.path.startsWith('.github/audit-tool/') ||
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
