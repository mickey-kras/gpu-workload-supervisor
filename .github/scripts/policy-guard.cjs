const YAML = require('yaml');
const { FORBIDDEN_INPUTS, LOCKFILES, inspectLockfiles } = require('./policy-lockfiles.cjs');
const { POLICY_SOURCES, reviewedHead, readPolicyConstant } = require('./policy-admission.cjs');
const { createHash } = require('node:crypto');
const { inspectReleaseEntry, inspectReleasePublish } = require('./policy-release.cjs');
const { normalizeParallelCi, AGGREGATE_RUN } = require('./policy-ci-parallel.cjs');
const { normalizeScopedValidation } = require('./policy-pr-scope.cjs');
const { normalizeCiReuse } = require('./policy-ci-reuse.cjs');
const { canonical, normalizeActionSteps, normalizeWorkflowActionPins } = require('./policy-action-pins.cjs');
const { inspectDependabot, verifyDependabotAuthor } = require('./policy-dependabot.cjs');

const GORELEASER_ACTION_DIGEST = 'c88133b8d7e69681883fe0aa77a141453ace73146561612522b9cd5f485722ac';

const EXECUTABLE_HASHES = {
  ".github/workflows/docs-ste.yml": "674f1c64f24b65cc50fcc5b1223c73ecfe0e607ed0e3d8a50b517fbb218fe4b4",
  ".ste.yml": "f75eda8fb8d6ae25db6875adf984b0bb167d52aff977ff5524bcc0f48d02c941",
  ".vale.ini": "1d2daa33c324e5140367a5143df339068822cb45f7f2ae023dbd7d744e0793ff",
  ".vale/styles/config/vocabularies/Project/accept.txt": "5000d8689ae7037cf1336122d156368bbcb9ec3da339d86e1a806f8ecb65668d",
  ".github/scripts/install-goreleaser.sh": "de2e876e51298aec0ec86a42fdb850aaa2f95e3d86cd1fa67a6d5c1febbcc8b0",
  ".github/scripts/check-go-directives.go": "f356bcb0bd9546d4ee133001335740bfb41623e4671554606895e6ab1a305489",
  ".github/scripts/eslint.config.cjs": "72d5ad0cda934ad013c3b1ddb62bf6aa1b5a65caa457f18c5f6725ea93e48f31"
};

function expectedExecutableDigest(files, path, admitted) {
  if (!admitted.has(path)) return EXECUTABLE_HASHES[path];
  try {
    const source = files['.github/scripts/policy-guard.cjs'] || '';
    return readPolicyConstant(source, 'EXECUTABLE_HASHES')?.[path];
  } catch {
    return undefined; // The caller rejects a missing or malformed digest.
  }
}

const STE_FILES = ['.github/workflows/docs-ste.yml', '.ste.yml', '.vale.ini', '.vale/styles/config/vocabularies/Project/accept.txt'];

const REQUIRED_FILES = [
  ...STE_FILES,
  '.github/CODEOWNERS',
  '.github/workflows/pr-validation.yml', '.github/workflows/ci.yml',
  '.github/workflows/main.yml', '.github/workflows/policy-guard.yml',
  '.github/workflows/release.yml', '.github/workflows/codeql.yml',
  '.github/workflows/aislop.yml', '.github/workflows/dependency-review.yml',
  '.github/workflows/branch-policy.yml', '.github/workflows/dependabot-auto-merge.yml',
  '.github/workflows/dependabot-auto-merge-refresh.yml', '.github/dependabot.yml',
  '.github/scripts/policy-guard.cjs', '.github/scripts/dependabot-auto-merge.cjs',
  '.github/scripts/release-settings.cjs', '.github/scripts/release-follow-up.cjs',
  'release-version.json', '.github/scripts/policy-release.cjs', '.github/scripts/policy-ci-parallel.cjs', '.github/scripts/policy-ci-reuse.cjs',
  '.github/scripts/policy-pr-scope.cjs', '.github/scripts/pr-validation-scope.cjs',
  '.github/scripts/policy-action-pins.cjs', '.github/scripts/policy-dependabot.cjs',
  '.github/scripts/policy-lockfiles.cjs', '.github/scripts/policy-admission.cjs',
  '.github/scripts/quality-coverage.py', '.github/scripts/check-scan-reports.py',
  '.github/scripts/release-validation.cjs', '.github/scripts/release-publication.cjs',
  '.github/scripts/pr-branch-updater.cjs', 'sonar-project.properties',
  '.github/scripts/package.json', '.github/scripts/package-lock.json',
  '.github/scripts/eslint.config.cjs', '.github/semgrep-tests/maintainability.go',
  '.github/scripts/check-go-directives.go', '.github/scripts/check-go-directives_test.go',
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

function inspectGoReleaser({ files, workflows, failures, checks, path, jobId, builds, expectedIf, admitted = new Set() }) {
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
  const actionPath = '.github/actions/setup-goreleaser/action.yml';
  try {
    const doc = YAML.parseDocument(files[actionPath] || '', { uniqueKeys: true });
    if (doc.errors.length) throw doc.errors[0];
    const action = doc.toJS();
    const controls = { ...action, runs: { ...action?.runs, steps: normalizeActionSteps(action?.runs?.steps) } };
    const expected = admitted.has(actionPath)
      ? readPolicyConstant(files['.github/scripts/policy-guard.cjs'], 'GORELEASER_ACTION_DIGEST')
      : GORELEASER_ACTION_DIGEST;
    if (createHash('sha256').update(JSON.stringify(canonical(controls))).digest('hex') !== expected) failures.push(`${actionPath} changed verified GoReleaser installer`);
  } catch {
    failures.push(`${actionPath} changed verified GoReleaser installer`);
  }
  const installer = '.github/scripts/install-goreleaser.sh';
  if (createHash('sha256').update(files[installer] || '').digest('hex') !==
      expectedExecutableDigest(files, installer, admitted)) {
    failures.push(`${installer} changed verified GoReleaser installer`);
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

function inspectCi(files, workflows, failures, checks, admitted) {
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
  step(ci, 'checks', 'Go cognitive complexity');
  exactRun(ci, 'checks', 'Go cognitive complexity', [
    'go run .github/scripts/check-go-directives.go .',
    "go run github.com/uudashr/gocognit/cmd/gocognit@v1.3.0 -over 15 -test=false -ignore '(^|/)(\\.github|docs|node_modules)/' .",
  ]);
  step(ci, 'checks', 'Test Go suppression guard');
  exactRun(ci, 'checks', 'Test Go suppression guard', [
    'go test .github/scripts/check-go-directives.go .github/scripts/check-go-directives_test.go',
  ]);
  step(ci, 'checks', 'JavaScript maintainability');
  exactRun(ci, 'checks', 'JavaScript maintainability', [
    '.github/scripts/node_modules/.bin/eslint --config .github/scripts/eslint.config.cjs --max-warnings 0 .',
  ]);
  step(ci, 'checks', 'Test setup shared-literal rule');
  exactRun(ci, 'checks', 'Test setup shared-literal rule', [
    'docker run --rm -v "${PWD}:/src" -w /src \\',
    'semgrep/semgrep:1.172.0@sha256:65dcd4408adda7c183a6b4550cb1e9b19f7f627a6fbb7e0559bd466bedc44d7b \\',
    'semgrep --test --config .semgrep.yml .github/semgrep-tests/maintainability.go',
  ]);
  inspectGoReleaser({ files, workflows, failures, checks, admitted, path: ci, jobId: 'checks', builds: [
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

}

function inspectAdditionalWorkflows(files, workflows, failures, checks, admitted) {
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
  const reviewInputs = workflows[dependency]?.jobs?.review?.steps?.find(s => s.name === 'Dependency review')?.with;
  if (!reviewInputs || Object.keys(reviewInputs).length !== 1 ||
      reviewInputs['config-file'] !== './.github/dependency-review-config.yml') {
    failures.push('Dependency review inputs must use the protected config without overrides');
  }
  inspectReleaseEntry(files, workflows, failures, checks, admitted);
  inspectReleasePublish(files, workflows, failures, checks, options => inspectGoReleaser({ ...options, admitted }));
  inspectDependabot(files, workflows, failures, checks);
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

function inspectScannerConfigs(files, workflows, failures, admitted) {
  for (const path of STE_FILES) {
    const content = path === '.github/workflows/docs-ste.yml'
      ? JSON.stringify(canonical(normalizeWorkflowActionPins(workflows[path] || {}))) : files[path] || '';
    if (createHash('sha256').update(content).digest('hex') !==
        expectedExecutableDigest(files, path, admitted)) {
      failures.push(`${path}: documentation gate controls changed`);
    }
  }
  if (createHash('sha256').update(files['.github/scripts/check-go-directives.go'] || '').digest('hex') !==
      expectedExecutableDigest(files, '.github/scripts/check-go-directives.go', admitted)) {
    failures.push('Go suppression guard was changed');
  }
  // Validate PR config bytes as data; the trusted guard must never execute it.
  const eslintConfig = files['.github/scripts/eslint.config.cjs'] || '';
  if (createHash('sha256').update(eslintConfig).digest('hex') !==
      expectedExecutableDigest(files, '.github/scripts/eslint.config.cjs', admitted)) {
    failures.push('JavaScript maintainability policy was changed');
  }
  try {
    const raw = files['.github/dependency-review-config.yml'];
    const config = YAML.parse(raw, { uniqueKeys: true });
    const keys = ['fail-on-severity', 'vulnerability-check', 'license-check', 'warn-only', 'fail-on-scopes'];
    const scopes = config?.['fail-on-scopes'];
    const current = config && Object.keys(config).length === keys.length &&
      keys.every(key => Object.hasOwn(config, key)) &&
      config['fail-on-severity'] === 'high' && config['vulnerability-check'] === true &&
      config['license-check'] === false && config['warn-only'] === false &&
      Array.isArray(scopes) && scopes.length === 3 &&
      ['runtime', 'development', 'unknown'].every(scope => scopes.includes(scope));
    if (!current) {
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
    const sharedLiterals = rules.find(rule => rule.id === 'go.setup-shared-literals');
    if (JSON.stringify(sharedLiterals) !== JSON.stringify({
      id: 'go.setup-shared-literals',
      message: 'Use a named constant for the shared ExecStart directive or inspection-failed status.',
      languages: ['go'], severity: 'ERROR',
      paths: { include: ['/internal/setup/*.go'], exclude: ['*_test.go'] },
      options: { constant_propagation: false },
      patterns: [
        { 'pattern-either': [{ pattern: '"ExecStart="' }, { pattern: '"inspection-failed"' }] },
        { 'pattern-not-inside': 'const $NAME = $VALUE' },
      ],
    })) failures.push('Setup shared-literal policy was weakened');
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
    const override = config.override;
    if (Object.keys(config).some(key => !['profile', 'threshold', 'override'].includes(key)) ||
        config.profile !== 'coverage.out' ||
        !validFloor(config.threshold?.total, 90) || !validFloor(config.threshold?.package, 90) ||
        !Array.isArray(override) || override.length !== floors.size ||
        new Set(override.map(rule => rule.path)).size !== override.length ||
        override.some(rule => !floors.has(rule.path) ||
          !validFloor(rule.threshold, floors.get(rule.path)))) {
      failures.push('Package coverage policy was weakened');
    }
  } catch (error) {
    failures.push(`Package coverage config is invalid: ${error.message}`);
  }
}

function onlyHardenRunnerPinsChanged(path, content, baseline) {
  if (path !== '.github/workflows/policy-guard.yml' ||
      typeof content !== 'string' || typeof baseline !== 'string') return false;
  const normalize = value => value.replace(
    /^([ \t]*-[ \t]+uses:[ \t]+step-security\/harden-runner@)[a-f0-9]{40}(?:[ \t]+#[^\r\n]*)?(?=\r?$)/gm,
    '$1PINNED',
  );
  return content !== baseline && normalize(content) === normalize(baseline);
}

function inspect(files, baseline = files, admitted = new Set()) {
  const failures = [];
  for (const path of POLICY_SOURCES) {
    const unreviewed = files[path] !== baseline[path] && !admitted.has(path) &&
      !onlyHardenRunnerPinsChanged(path, files[path], baseline[path]);
    if (typeof baseline[path] !== 'string' || unreviewed) {
      failures.push(`${path}: policy execution sources require independent exact-head owner admission`);
    }
  }
  const scoped = normalizeScopedValidation(files, scanWorkflows(files, failures), failures, AGGREGATE_RUN + '\n', admitted);
  const workflows = normalizeCiReuse(files, scoped, failures, admitted);
  const checks = createChecks(workflows, failures);
  const ciWorkflows = normalizeParallelCi(workflows, failures);
  inspectCi(files, ciWorkflows, failures, createChecks(ciWorkflows, failures), admitted);
  inspectAdditionalWorkflows(files, workflows, failures, checks, admitted);
  inspectScannerConfigs(files, workflows, failures, admitted);
  inspectLockfiles(files, baseline, failures, admitted);
  inspectCoverageConfig(files, failures);
  return failures;
}

async function run({ github, context }) {
  const { owner, repo } = context.repo;
  const pullParams = { owner, repo, pull_number: context.payload.pull_request.number };
  const { data: initial } = await github.rest.pulls.get(pullParams);
  const eventPull = context.payload.pull_request;
  if (initial.state !== 'open' || initial.head?.sha !== eventPull.head.sha || initial.base?.sha !== eventPull.base.sha) {
    throw new Error('Pull request head or base changed; rerun policy for the current commit');
  }
  const sha = context.payload.pull_request.head.sha;
  const { data: commit } = await github.rest.git.getCommit({ owner, repo, commit_sha: sha });
  const { data: tree } = await github.rest.git.getTree({
    owner, repo, tree_sha: commit.tree.sha, recursive: 'true',
  });
  if (tree.truncated) throw new Error('Cannot verify a truncated PR tree');
  for (const entry of tree.tree) {
    if (FORBIDDEN_INPUTS.includes(entry.path)) throw new Error(`${entry.path}: CI npm input redirection is forbidden`);
    if (REQUIRED_FILES.includes(entry.path) &&
        (entry.type !== 'blob' || !['100644', '100755'].includes(entry.mode))) {
      throw new Error(`${entry.path}: policy inputs must be regular files`);
    }
  }
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
  const baseline = {};
  for (const path of [...POLICY_SOURCES, ...LOCKFILES.flatMap(path => [path, path.replace('package-lock.json', 'package.json')])]) {
    const { data } = await github.rest.repos.getContent({
      owner, repo, path, ref: context.payload.pull_request.base.sha,
    });
    if (data.type !== 'file' || data.encoding !== 'base64' || typeof data.content !== 'string') {
      throw new Error(`Cannot read immutable base dependency input: ${path}`);
    }
    baseline[path] = Buffer.from(data.content, 'base64').toString('utf8');
  }
  let authorFailure;
  try { await verifyDependabotAuthor(github, context); } catch (error) { authorFailure = error; }
  let failures = inspect(files, baseline);
  if ((authorFailure || failures.length) && await reviewedHead(github, context)) {
    failures = inspect(files, baseline, new Set(Object.keys(files)));
    await verifyDependabotAuthor(github, context, true);
    authorFailure = undefined;
  }
  if (authorFailure) throw authorFailure;
  if (failures.length) throw new Error(failures.join('\n'));
  const { data: final } = await github.rest.pulls.get(pullParams);
  if (final.state !== 'open' || final.head?.sha !== eventPull.head.sha || final.base?.sha !== eventPull.base.sha) {
    throw new Error('Pull request head or base changed during policy inspection');
  }
}

module.exports = { inspect, run, verifyDependabotAuthor };
