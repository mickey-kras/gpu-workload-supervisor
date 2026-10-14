const test = require('node:test');
const assert = require('node:assert/strict');
const { readFileSync, readdirSync } = require('node:fs');
const { inspect: inspectActivated } = require('./policy-guard.cjs');
const { runInNewContext } = require('node:vm');
const parallelPolicy = require('./policy-ci-parallel.cjs');
const scopePolicy = require('./policy-pr-scope.cjs');

// These serial fixtures isolate the original command checks. Production and
// activation regressions below always use the real, strict graph inspector.
const commandModule = { exports: {} };
runInNewContext(readFileSync('.github/scripts/policy-guard.cjs', 'utf8'), {
  module: commandModule, exports: commandModule.exports, Buffer,
  require: name => name === './policy-ci-parallel.cjs' ? {
    normalizeParallelCi(workflows, failures) {
      const jobs = workflows['.github/workflows/ci.yml']?.jobs;
      return jobs && Object.keys(jobs).length === 1 ? workflows :
        parallelPolicy.normalizeParallelCi(workflows, failures);
    },
  } : name === './policy-pr-scope.cjs' ? {
    normalizeScopedValidation(files, workflows, failures, aggregate) {
      const jobs = workflows['.github/workflows/ci.yml']?.jobs;
      return jobs && Object.keys(jobs).length === 1 ? workflows :
        scopePolicy.normalizeScopedValidation(files, workflows, failures, aggregate);
    },
  } : require(name),
});
const inspect = candidate => Object.keys(YAML.parse(candidate['.github/workflows/ci.yml'] || '')?.jobs || {}).length === 1 ?
  Array.from(commandModule.exports.inspect(candidate)) : inspectActivated(candidate);
const YAML = require('yaml');
const { VALIDATION_SCRIPT } = require('./policy-release.cjs');

function files({ serial = true } = {}) {
  const result = {};
  for (const name of readdirSync('.github/workflows')) {
    if (name.endsWith('.yml')) {
      result[`.github/workflows/${name}`] = readFileSync(`.github/workflows/${name}`, 'utf8');
    }
  }
  for (const path of [
    '.github/dependabot.yml', '.github/scripts/policy-guard.cjs',
    '.github/scripts/dependabot-auto-merge.cjs', '.github/scripts/pr-branch-updater.cjs',
    '.github/scripts/release-settings.cjs', '.github/scripts/release-follow-up.cjs',
    'release-version.json', '.github/scripts/policy-release.cjs', '.github/scripts/policy-ci-parallel.cjs', '.github/scripts/policy-ci-reuse.cjs',
    '.github/scripts/policy-pr-scope.cjs', '.github/scripts/pr-validation-scope.cjs',
    '.github/scripts/policy-action-pins.cjs', '.github/scripts/policy-dependabot.cjs',
    '.github/scripts/quality-coverage.py', '.github/scripts/check-scan-reports.py',
    '.github/scripts/release-validation.cjs', '.github/scripts/release-publication.cjs',
    '.github/scripts/package.json', '.github/scripts/package-lock.json',
    '.github/scripts/eslint.config.cjs', '.github/semgrep-tests/maintainability.go',
    '.github/scripts/check-go-directives.go', '.github/scripts/check-go-directives_test.go',
    '.github/aislop/package.json', '.github/aislop/package-lock.json',
    '.github/dependency-review-config.yml', '.semgrep.yml', '.aislop/config.yml',
    'sonar-project.properties', '.goreleaser.yaml', '.testcoverage.yml',
    '.github/actions/setup-goreleaser/action.yml', '.github/scripts/install-goreleaser.sh',
  ]) result[path] = readFileSync(path, 'utf8');
  // Legacy mutation tests still exercise the original command controls in a
  // serial fixture; the actual parallel workflow is separately checked below.
  const path = '.github/workflows/ci.yml';
  let source = result[path];
  if (serial && YAML.parse(source).on.workflow_call?.inputs) {
    const paths = Object.keys(result).filter(path => path.startsWith('.github/workflows/'));
    const workflows = Object.fromEntries(paths.map(path => [path, YAML.parse(result[path])]));
    const normalized = scopePolicy.normalizeScopedValidation(result, workflows, [], parallelPolicy.AGGREGATE_RUN + '\n');
    for (const changed of [path, '.github/workflows/pr-validation.yml']) result[changed] = YAML.stringify(normalized[changed]);
    source = result[path];
  }
  if (serial && YAML.parse(source).on.workflow_call?.outputs) {
    const { normalizeCiReuse } = require('./policy-ci-reuse.cjs');
    const paths = ['ci', 'main', 'aislop'].map(name => `.github/workflows/${name}.yml`);
    const workflows = Object.fromEntries(paths.map(path => [path, YAML.parse(result[path])]));
    const normalized = normalizeCiReuse(result, workflows, []);
    for (const path of paths) result[path] = YAML.stringify(normalized[path]);
    source = result[path];
  }
  if (serial && YAML.parse(source).jobs.native) {
    const jobsIndex = source.indexOf('jobs:\n');
    const sections = source.slice(jobsIndex + 6).split(/^  [a-z]+:\n/m).slice(1);
    const steps = sections.map(section => section.slice(section.indexOf('    steps:\n') + 11)).join('');
    result[path] = source.slice(0, jobsIndex) + 'jobs:\n  checks:\n    name: checks\n    runs-on: ubuntu-latest\n    steps:\n' + steps;
  }
  return result;
}

function scopedCandidate() {
  const controls = require('./policy-pr-scope.cjs');
  const candidate = files({ serial: false });
  const ciPath = '.github/workflows/ci.yml';
  const prPath = '.github/workflows/pr-validation.yml';
  const ci = YAML.parse(candidate[ciPath]);
  const pr = YAML.parse(candidate[prPath]);
  ci.on.workflow_call.inputs = structuredClone(controls.SCOPE_INPUT);
  ci.jobs.native.if = controls.FULL_IF;
  for (const [job, names] of [['go', controls.GO_STEPS], ['checks', controls.CANDIDATE_STEPS]]) {
    for (const name of names) ci.jobs[job].steps.find(step => step.name === name).if = controls.FULL_IF;
  }
  const gate = ci.jobs.checks.steps.find(step => step.name === 'Require every CI job to succeed');
  gate.run = controls.SCOPED_AGGREGATE;
  gate.env = structuredClone(controls.AGGREGATE_ENV);
  pr.jobs.classify = structuredClone(controls.CLASSIFIER_JOB);
  pr.jobs.quality = structuredClone(controls.SCOPED_QUALITY);
  candidate[ciPath] = YAML.stringify(ci);
  candidate[prPath] = YAML.stringify(pr);
  return { candidate, ciPath, prPath, ci, pr };
}

test('trusted docs routing preserves the full scanner and coverage reuse profile', () => {
  const { candidate } = scopedCandidate();
  assert.deepEqual(inspectActivated(candidate), []);
});

test('docs routing cannot skip audits, packaged documentation, security analyses or full coverage uploads', () => {
  for (const [job, name] of [
    ['go', 'Go vulnerability audit'], ['frontend', 'Audit Aislop toolchain'],
    ['scans', 'Gitleaks'], ['scans', 'Semgrep'], ['scans', 'Convert Trivy SARIF'],
    ['scans', 'Trivy high and critical gate'], ['package', 'Build snapshot artifacts'],
    ['frontend', 'Upload javascript quality coverage'],
  ]) {
    const state = scopedCandidate();
    state.ci.jobs[job].steps.find(step => step.name === name).if = false;
    state.candidate[state.ciPath] = YAML.stringify(state.ci);
    assert.notDeepEqual(inspectActivated(state.candidate), [], name);
  }
  for (const mutate of [
    state => { state.pr.jobs.classify.steps[1].with.ref = '${{ github.sha }}'; },
    state => { state.pr.jobs.quality.with['validation-mode'] = 'docs-only'; },
    state => { state.ci.jobs.go.steps.find(step => step.name === 'Upload go quality coverage').if = false; },
    state => { state.ci.jobs.checks.steps.find(step => step.name === 'Upload CI-tested candidate artifacts').if = undefined; },
  ]) {
    const state = scopedCandidate(); mutate(state);
    state.candidate[state.ciPath] = YAML.stringify(state.ci);
    state.candidate[state.prPath] = YAML.stringify(state.pr);
    assert.notDeepEqual(inspectActivated(state.candidate), []);
  }
});

test('approved scanner and coverage reuse preserves every existing quality gate', () => {
  const { reuseFixture } = require('./ci-reuse-fixture.cjs');
  const candidate = files({ serial: false });
  for (const [path, workflow] of Object.entries(reuseFixture())) candidate[path] = YAML.stringify(workflow);
  assert.deepEqual(inspect(candidate), []);
});

test('parallel CI preserves all existing trusted command and scanner gates', () => {
  const { parallelFixture } = require('./ci-parallel-fixture.cjs');
  const candidate = files();
  const path = '.github/workflows/ci.yml';
  const workflow = parallelFixture();
  candidate[path] = YAML.stringify(workflow);
  assert.deepEqual(inspect(candidate), []);
  for (const [id, name] of [
    ['go', 'Tests, race detector, and coverage'], ['go', 'Go cognitive complexity'],
    ['frontend', 'Test policy automation'], ['scans', 'Semgrep'], ['package', 'Build snapshot artifacts'],
  ]) {
    const changed = structuredClone(workflow);
    changed.jobs[id].steps.find(step => step.name === name).run = 'true';
    candidate[path] = YAML.stringify(changed);
    assert.ok(inspect(candidate).some(error => error.includes(name)), name);
  }
});

test('current governance workflows satisfy the trusted guard', () => {
  assert.deepEqual(inspectActivated(files({ serial: false })), []);
});

test('activated policy rejects returning to a serial or single-checks graph', () => {
  for (const downgrade of ['serial', 'quick', 'unqualified-candidate']) {
    const candidate = files();
    const path = '.github/workflows/ci.yml';
    const workflow = YAML.parse(candidate[path]);
    const steps = workflow.jobs.checks.steps;
    if (downgrade === 'quick') {
      steps.find(step => step.name === 'Native GTK setup, accessibility and screenshot matrix').run =
        'python3 tests/desktop/setup_native.py --quick';
    } else if (downgrade === 'unqualified-candidate') {
      const upload = steps.splice(steps.findIndex(step => step.name === 'Upload CI-tested candidate artifacts'), 1)[0];
      steps.unshift(upload);
      steps.splice(steps.findIndex(step => step.name === 'Packaged setup lifecycle integration'), 1);
    }
    candidate[path] = YAML.stringify(workflow);
    assert.ok(inspectActivated(candidate).includes('Parallel CI lost required job set'), downgrade);
  }
});

function reusableRelease(candidate) {
  const path = '.github/workflows/release.yml';
  const workflow = YAML.parse(candidate[path]);
  for (const id of ['quality', 'aislop', 'codeql']) delete workflow.jobs[id];
  const script = VALIDATION_SCRIPT;
  const verify = { name: 'Verify exact-commit main validation',
    uses: 'actions/github-script@3a2844b7e9c422d3c10d287c895573f7108da1b3',
    env: { VERSION: '${{ needs.entry.outputs.version }}' }, with: { script } };
  const entrySteps = workflow.jobs.entry.steps;
  const harden = entrySteps.find(step => step.uses?.startsWith('step-security/harden-runner@'));
  workflow.jobs.validation = { needs: 'entry', 'runs-on': 'ubuntu-latest', 'timeout-minutes': 75,
    permissions: { contents: 'read', actions: 'read' }, steps: [
      structuredClone(harden),
      { uses: 'actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1',
        with: { ref: '${{ github.sha }}', 'persist-credentials': false } },
      verify,
    ] };
  workflow.jobs.publish.needs = ['entry', 'validation'];
  const publishVerify = workflow.jobs.publish.steps.find(step =>
    step.name === 'Verify main and successful validation at this commit');
  publishVerify.with.script = script;
  workflow.jobs.publish.steps.find(step => step.uses?.startsWith('actions/checkout@')).with.ref = '${{ github.sha }}';
  return { path, workflow, verify, publishVerify };
}

test('release validation policy accepts strict reuse at the protected main-validation gate', () => {
  const candidate = files({ serial: false }); const { path, workflow } = reusableRelease(candidate);
  candidate[path] = YAML.stringify(workflow);
  assert.deepEqual(inspect(candidate), []);
});

test('release validation reuse cannot skip or weaken its exact-commit gate', () => {
  for (const mutate of [
    state => { state.workflow.jobs.publish.needs = ['entry']; },
    state => { state.workflow.jobs.validation.needs = []; },
    state => { state.workflow.jobs.validation.if = false; },
    state => { state.workflow.jobs.validation.permissions.actions = 'write'; },
    state => { state.workflow.jobs.validation['timeout-minutes'] = 10; },
    state => { state.workflow.jobs.publish['runs-on'] = 'self-hosted'; },
    state => { state.workflow.jobs.validation.steps[1].with.ref = 'main'; },
    state => { state.workflow.jobs.validation.steps.splice(2, 0, { run: 'npm install semver' }); },
    state => { state.verify.if = false; },
    state => { state.verify.with.script = 'console.log("validation skipped")'; },
    state => { state.verify.env.VERSION = '0.1.0'; },
    state => { state.verify.with['github-token'] = '${{ secrets.WRITE_TOKEN }}'; },
    state => { state.publishVerify.with.script = 'return;'; },
    state => { state.publishVerify.if = false; },
  ]) {
    const candidate = files({ serial: false }); const state = reusableRelease(candidate); mutate(state);
    candidate[state.path] = YAML.stringify(state.workflow);
    assert.notDeepEqual(inspect(candidate), []);
  }
});

test('release verifier bytes are protected at the reused main gate', () => {
  const candidate = files({ serial: false }); candidate['.github/scripts/release-validation.cjs'] += '\nreturn;\n';
  assert.ok(inspect(candidate).some(error => error.includes('verifier was changed')));
  const missing = files({ serial: false }); delete missing['.github/scripts/release-validation.cjs'];
  assert.notDeepEqual(inspect(missing), []);
  const helper = files({ serial: false }); helper['.github/scripts/release-publication.cjs'] = 'process.exit(0);';
  assert.ok(inspect(helper).some(error => error.includes('publication helper was changed')));
});

test('verification jobs reject workspace writes and runtime or wrapper input overrides', () => {
  for (const mutate of [
    state => { state.workflow.jobs.validation.steps.splice(2, 0, {
      run: "printf 'module.exports = { verifyMain: async () => {} };' > .github/scripts/release-validation.cjs",
    }); },
    state => { state.workflow.jobs.publish.steps.splice(4, 0, {
      run: "printf 'module.exports = { verifyMain: async () => {} };' > .github/scripts/release-validation.cjs",
    }); },
    state => { state.workflow.env = { NODE_OPTIONS: '--require ./malicious.cjs' }; },
    state => { state.workflow.jobs.validation.env = { NODE_OPTIONS: '--require ./malicious.cjs' }; },
    state => { state.workflow.jobs.publish.env = { NODE_OPTIONS: '--require ./malicious.cjs' }; },
    state => { state.verify.env.NODE_OPTIONS = '--require ./malicious.cjs'; },
    state => { state.verify.with['github-token'] = '${{ secrets.WRITE_TOKEN }}'; },
    state => { state.workflow.jobs.validation.steps[1].env = { NODE_OPTIONS: '--require ./malicious.cjs' }; },
    state => { state.workflow.jobs.publish.steps[3].env = { NODE_OPTIONS: '--require ./malicious.cjs' }; },
    state => { state.workflow.jobs.validation.defaults = { run: { 'working-directory': 'other' } }; },
  ]) {
    const candidate = files({ serial: false }); const state = reusableRelease(candidate); mutate(state);
    candidate[state.path] = YAML.stringify(state.workflow);
    assert.ok(inspect(candidate).some(error => error.includes('workspace or runtime inputs')));
  }
});

test('retired duplicate release validation cannot replace the trusted main gate', () => {
  const candidate = files({ serial: false });
  const path = '.github/workflows/release.yml';
  const workflow = YAML.parse(candidate[path]);
  delete workflow.jobs.validation;
  workflow.jobs.publish.needs = ['entry', 'quality', 'aislop', 'codeql'];
  candidate[path] = YAML.stringify(workflow);
  assert.ok(inspect(candidate).some(error => error.includes('lost exact-commit main validation')));
});

test('maintainability gates cannot be removed, skipped or weakened', () => {
  for (const name of ['Go cognitive complexity', 'Test Go suppression guard', 'JavaScript maintainability', 'Test setup shared-literal rule']) {
    for (const mutation of ['missing', 'conditional', 'advisory', 'command']) {
      const candidate = files();
      const path = '.github/workflows/ci.yml';
      const workflow = YAML.parse(candidate[path]);
      const steps = workflow.jobs.checks.steps;
      const index = steps.findIndex(step => step.name === name);
      if (mutation === 'missing') steps.splice(index, 1);
      else if (mutation === 'conditional') steps[index].if = false;
      else if (mutation === 'advisory') steps[index]['continue-on-error'] = true;
      else steps[index].run = 'echo skipped';
      candidate[path] = YAML.stringify(workflow);
      assert.ok(inspect(candidate).some(error => error.includes(name) || error.includes('may ignore')));
    }
  }
  const candidate = files();
  candidate['.github/workflows/ci.yml'] = candidate['.github/workflows/ci.yml'].replace('-over 15', '-over 16');
  assert.ok(inspect(candidate).some(error => error.includes('Go cognitive complexity')));
});

test('Go directive rejection cannot be removed or changed to allow suppressions', () => {
  const candidate = files();
  candidate['.github/workflows/ci.yml'] = candidate['.github/workflows/ci.yml']
    .replace('          go run .github/scripts/check-go-directives.go .\n', '');
  assert.ok(inspect(candidate).some(error => error.includes('Go cognitive complexity')));
  for (const mutation of [
    undefined,
    'package main\nfunc main() {}\n',
    files()['.github/scripts/check-go-directives.go']
      .replace('excludedGoPaths.MatchString(adjusted.Filename)', 'false'),
  ]) {
    const changed = files();
    if (mutation === undefined) delete changed['.github/scripts/check-go-directives.go'];
    else changed['.github/scripts/check-go-directives.go'] = mutation;
    assert.ok(inspect(changed).some(error => error.includes('Go suppression guard')));
  }
});

test('PR JavaScript config cannot remove rules, add ignores or execute in the trusted guard', () => {
  for (const mutate of [
    source => source.replace("'error'", "'off'"),
    source => source.replace("ignores: [", "ignores: ['clients/setup/**',"),
    source => source + '\nthrow new Error("must not run");\n',
  ]) {
    const candidate = files();
    candidate['.github/scripts/eslint.config.cjs'] = mutate(candidate['.github/scripts/eslint.config.cjs']);
    assert.ok(inspect(candidate).some(error => error.includes('JavaScript maintainability policy')));
  }
});

test('setup shared-literal policy cannot ignore production files or allow inlined literals', () => {
  for (const mutate of [
    rule => { rule.paths.exclude.push('application_discovery.go'); },
    rule => { rule.patterns[0]['pattern-either'].pop(); },
    rule => { rule.severity = 'WARNING'; },
  ]) {
    const candidate = files();
    const config = YAML.parse(candidate['.semgrep.yml']);
    mutate(config.rules.find(rule => rule.id === 'go.setup-shared-literals'));
    candidate['.semgrep.yml'] = YAML.stringify(config);
    assert.ok(inspect(candidate).some(error => error.includes('Setup shared-literal policy')));
  }
});

test('GoReleaser cannot revert to optional signature verification', () => {
  for (const [path, jobId] of [
    ['.github/workflows/ci.yml', 'checks'], ['.github/workflows/release.yml', 'publish'],
  ]) {
    const candidate = files();
    const workflow = YAML.parse(candidate[path]);
    const steps = workflow.jobs[jobId].steps;
    workflow.jobs[jobId].steps = steps.filter(s => s.name !== 'Install verified GoReleaser');
    for (const step of steps.filter(s => s.run?.startsWith('goreleaser '))) {
      step.uses = 'goreleaser/goreleaser-action@f06c13b6b1a9625abc9e6e439d9c05a8f2190e94';
      step.with = { version: 'v2.18.2', args: step.run.trim().slice('goreleaser '.length) };
      delete step.run;
    }
    candidate[path] = YAML.stringify(workflow);
    assert.ok(inspect(candidate).some(error => error.includes('GoReleaser')));
  }
});

test('vulnerability audit cannot restore the Go cache a second time', () => {
  const candidate = files();
  candidate['.github/workflows/ci.yml'] = candidate['.github/workflows/ci.yml']
    .replace('          repo-checkout: false\n          cache: false', '          repo-checkout: false\n          cache: true');
  assert.ok(inspect(candidate).some(error => error.includes('Go vulnerability audit')));
});

test('verified GoReleaser route rejects changed installer source or action commands', () => {
  for (const path of ['.github/actions/setup-goreleaser/action.yml', '.github/scripts/install-goreleaser.sh']) {
    const candidate = files();
    candidate[path] = path.endsWith('.yml') ? candidate[path].replace('v2.18.2', 'v0.0.0') : candidate[path] + '\n# changed\n';
    assert.ok(inspect(candidate).some(error => error.includes('changed verified GoReleaser installer')));
  }
});

test('verified GoReleaser setup cannot be missing, conditional, or after the build', () => {
  for (const mutation of ['missing', 'conditional', 'late']) {
    const candidate = files();
    const path = '.github/workflows/ci.yml';
    const workflow = YAML.parse(candidate[path]);
    const steps = workflow.jobs.checks.steps;
    const index = steps.findIndex(s => s.name === 'Install verified GoReleaser');
    if (mutation === 'conditional') steps[index].if = false;
    else {
      const [setup] = steps.splice(index, 1);
      if (mutation === 'late') steps.push(setup);
    }
    candidate[path] = YAML.stringify(workflow);
    assert.ok(inspect(candidate).some(error => error.includes('GoReleaser')));
  }
});

test('verified GoReleaser commands and release condition cannot be weakened', () => {
  const candidate = files();
  candidate['.github/workflows/ci.yml'] = candidate['.github/workflows/ci.yml']
    .replace('run: goreleaser check', 'run: echo skipped');
  assert.ok(inspect(candidate).some(error => error.includes('changed gate commands')));
  const release = files();
  release['.github/workflows/release.yml'] = release['.github/workflows/release.yml']
    .replaceAll("steps.state.outputs.published != 'true'", 'false');
  assert.ok(inspect(release).some(error => error.includes('Install verified GoReleaser')));
});

test('disabled scanner with the original text retained in comments fails', () => {
  const candidate = files();
  candidate['.github/workflows/ci.yml'] = candidate['.github/workflows/ci.yml']
    .replace('      - name: Gitleaks\n        uses:', '      # - name: Gitleaks\n      #   uses:');
  assert.ok(inspect(candidate).some(error => error.includes('Gitleaks')));
});

test('a mutable container action is rejected', () => {
  const candidate = files();
  candidate['.github/workflows/ci.yml'] += '\n# additional job\n';
  candidate['.github/workflows/ci.yml'] = candidate['.github/workflows/ci.yml']
    .replace('    steps:\n', '    steps:\n      - uses: docker://alpine:latest\n');
  assert.ok(inspect(candidate).some(error => error.includes('unpinned action')));
});

test('a scanner cannot be made advisory with continue-on-error', () => {
  const candidate = files();
  candidate['.github/workflows/ci.yml'] = candidate['.github/workflows/ci.yml']
    .replace('      - name: Gitleaks\n', '      - name: Gitleaks\n        continue-on-error: true\n');
  assert.ok(inspect(candidate).some(error => error.includes('may ignore step failures')));
});

test('early exit before retained test commands fails policy', () => {
  const candidate = files();
  candidate['.github/workflows/ci.yml'] = candidate['.github/workflows/ci.yml']
    .replace('          go test -race -coverprofile=coverage.out ./...',
      '          exit 0\n          go test -race -coverprofile=coverage.out ./...');
  assert.ok(inspect(candidate).some(error => error.includes('changed gate commands: Tests')));
});

test('a custom shell cannot ignore a protected run script', () => {
  const candidate = files();
  candidate['.github/workflows/ci.yml'] = candidate['.github/workflows/ci.yml']
    .replace('      - name: Tests, race detector, and coverage\n',
      "      - name: Tests, race detector, and coverage\n        shell: bash -c 'exit 0' {0}\n");
  assert.ok(inspect(candidate).some(error => error.includes('Tests, race detector, and coverage')));
});

test('a boolean false condition cannot skip a required scanner', () => {
  const candidate = files();
  candidate['.github/workflows/ci.yml'] = candidate['.github/workflows/ci.yml']
    .replace('      - name: Gitleaks\n', '      - name: Gitleaks\n        if: false\n');
  assert.ok(inspect(candidate).some(error => error.includes('Gitleaks')));
});

test('reviewed release settings cannot be skipped', () => {
  const candidate = files();
  const path = '.github/workflows/release.yml';
  candidate[path] = candidate[path].replace('await require(\'./.github/scripts/release-settings.cjs\').verify({', 'await Promise.resolve({');
  assert.ok(inspect(candidate).some(error => error.includes('reviewed release settings verification')));
});

test('version bump cannot run before publication succeeds', () => {
  const candidate = files();
  candidate['.github/workflows/release.yml'] = candidate['.github/workflows/release.yml']
    .replace('    needs: [entry, publish]', '    needs: entry');
  assert.ok(inspect(candidate).some(error => error.includes('post-publication version bump ordering')));
});

test('version bump cannot be skipped or use an arbitrary version', () => {
  const path = '.github/workflows/release.yml';
  const candidate = files();
  candidate[path] = candidate[path].replace('      - name: Queue narrowly validated next patch bump\n',
    '      - name: Queue narrowly validated next patch bump\n        if: false\n');
  assert.ok(inspect(candidate).some(error => error.includes('Queue narrowly validated')));
  candidate[path] = files()[path].replaceAll('          VERSION: ${{ needs.entry.outputs.version }}',
    '          VERSION: 1.0.0');
  assert.ok(inspect(candidate).some(error => error.includes('published bump version')));
});

test('reviewed release settings cannot be conditional or hidden in a comment', () => {
  const candidate = files();
  const path = '.github/workflows/release.yml';
  candidate[path] = candidate[path].replace('        id: state\n', '        id: state\n        if: false\n');
  assert.ok(inspect(candidate).some(error => error.includes('reviewed release settings verification')));
  candidate[path] = files()[path].replace("            await require('./.github/scripts/release-settings.cjs')",
    "            // await require('./.github/scripts/release-settings.cjs')");
  assert.ok(inspect(candidate).some(error => error.includes('reviewed release settings verification')));
});

test('scanner policy cannot become empty while workflow remains active', () => {
  const candidate = files();
  candidate['.semgrep.yml'] = 'rules: []\n';
  assert.ok(inspect(candidate).some(error => error.includes('Semgrep policy was weakened')));
});

test('deployable binaries cannot be removed from the release', () => {
  const candidate = files();
  candidate['.goreleaser.yaml'] = candidate['.goreleaser.yaml'].replace('main: ./cmd/gpu-workload-proxy', 'main: ./cmd/gpu-mode');
  assert.ok(inspect(candidate).some(error => error.includes('GoReleaser lost deployable')));
});

test('removed policy files and unpinned actions fail closed', () => {
  const errors = inspect({
    '.github/workflows/example.yml': 'jobs:\n  example:\n    steps:\n      - uses: actions/checkout@v7\n',
  });
  assert.ok(errors.some(error => error.includes('Missing required file')));
  assert.ok(errors.some(error => error.includes('unpin')));
});


test('package coverage gate cannot be deleted, skipped or made advisory', () => {
  for (const mutation of ['deleted', 'skipped', 'advisory', 'command']) {
    const candidate = files();
    const path = '.github/workflows/ci.yml';
    const workflow = YAML.parse(candidate[path]);
    const steps = workflow.jobs.checks.steps;
    const index = steps.findIndex(step => step.name === 'Package coverage floors');
    if (mutation === 'deleted') steps.splice(index, 1);
    if (mutation === 'skipped') steps[index].if = false;
    if (mutation === 'advisory') steps[index]['continue-on-error'] = true;
    if (mutation === 'command') steps[index].run += ' || true';
    candidate[path] = YAML.stringify(workflow);
    assert.notDeepEqual(inspect(candidate), [], mutation);
  }
});

test('package coverage floors cannot be lowered or bypassed with exclusions', () => {
  for (const mutate of [
    config => { config.threshold.total = 89; },
    config => { config.threshold.package = 89; },
    config => { config.override[1].threshold = 84; },
    config => { config.override[1].threshold = '85'; },
    config => { config.override[1].path = '^internal/'; },
    config => { config.override.push({ path: '.*', threshold: 0 }); },
    config => { config.override[1] = config.override[0]; },
    config => { config.exclude = { paths: ['internal/store'] }; },
    config => { config.profile = 'other.out'; },
  ]) {
    const candidate = files();
    const config = YAML.parse(candidate['.testcoverage.yml']);
    mutate(config);
    candidate['.testcoverage.yml'] = YAML.stringify(config);
    assert.ok(inspect(candidate).some(error => error.includes('Package coverage')));
  }
});

test('package coverage config rejects deletion and duplicate keys', () => {
  for (const value of [undefined, 'profile: coverage.out\nprofile: other.out']) {
    const candidate = files();
    candidate['.testcoverage.yml'] = value;
    assert.ok(inspect(candidate).some(error => error.includes('coverage')));
  }
});

function temporaryAuditFiles() {
  const candidate = files();
  candidate['.github/workflows/ci.yml'] = candidate['.github/workflows/ci.yml'].replace(
    '          npm ci --prefix .github/aislop --ignore-scripts --no-audit --no-fund\n          npm audit --prefix .github/aislop --audit-level=moderate',
    [
      'npm ci --prefix .github/audit-tool --ignore-scripts --no-audit --no-fund',
      'npm audit --prefix .github/audit-tool --audit-level=moderate',
      'npm ci --prefix .github/aislop --ignore-scripts --no-audit --no-fund',
      'node --test .github/audit-tool/audit.test.cjs',
      'node .github/audit-tool/audit.cjs .github/aislop',
    ].map(line => '          ' + line).join('\n'));
  return candidate;
}

test('retired temporary audit exception cannot replace the full audit', () => {
  assert.ok(inspect(temporaryAuditFiles()).some(error => error.includes('changed gate commands')));
});

function guardTrivyUpload(candidate) {
  const path = '.github/workflows/ci.yml';
  const workflow = YAML.parse(candidate[path]);
  const steps = workflow.jobs.checks.steps;
  const report = steps.find(step => step.name === 'Trivy filesystem report');
  const upload = steps.find(step => step.name === 'Upload Trivy SARIF');
  report.id = 'trivy_report';
  upload.if = "${{ !cancelled() && steps.trivy_report.outcome == 'success' && (github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository) }}";
  return { path, workflow, steps, report, upload };
}

test('Trivy upload rejects legacy and accepts successful-report conditions', () => {
  const legacy = files();
  const legacyPath = '.github/workflows/ci.yml';
  const legacyWorkflow = YAML.parse(legacy[legacyPath]);
  const legacySteps = legacyWorkflow.jobs.checks.steps;
  legacySteps.find(step => step.name === 'Upload Trivy SARIF').if = "${{ !cancelled() && (github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository) }}";
  legacy[legacyPath] = YAML.stringify(legacyWorkflow);
  assert.ok(inspect(legacy).some(error => error.includes('Upload Trivy SARIF')));
  const candidate = files();
  const { path, workflow } = guardTrivyUpload(candidate);
  candidate[path] = YAML.stringify(workflow);
  assert.deepEqual(inspect(candidate), []);
});

test('guarded Trivy upload requires the exact unconditional report producer', () => {
  for (const mutation of [
    state => { state.report.id = 'wrong'; },
    state => { state.report.if = false; },
    state => { state.report['continue-on-error'] = true; },
    state => { state.report.uses = 'actions/checkout@' + 'a'.repeat(40); },
    state => { state.report.with.output = 'other.sarif'; },
    state => { state.report.with.format = 'table'; },
    state => { state.report.with['scan-ref'] = 'empty'; },
    state => { state.report.with['exit-code'] = '1'; },
    state => { state.upload.with.sarif_file = 'other.sarif'; },
    state => { state.upload.if = state.upload.if.replace("outcome == 'success'", "outcome != 'skipped'"); },
    state => { state.steps.push({ id: 'trivy_report', run: 'true' }); },
    state => { state.steps.splice(state.steps.indexOf(state.report), 1); },
    state => { state.steps.splice(state.steps.indexOf(state.report), 1); state.steps.push(state.report); },
  ]) {
    const candidate = files();
    const state = guardTrivyUpload(candidate);
    mutation(state);
    candidate[state.path] = YAML.stringify(state.workflow);
    assert.notDeepEqual(inspect(candidate), []);
  }
});


test('desktop packages cannot be omitted from staging, checksums, or provenance', () => {
  for (const name of ['Build desktop packages', 'Checksum all release assets', 'Attest release assets']) {
    const candidate = files();
    const path = '.github/workflows/release.yml';
    const workflow = YAML.parse(candidate[path]);
    const step = workflow.jobs.publish.steps.find(item => item.name === name);
    if (name === 'Build desktop packages') {
      workflow.jobs.publish.steps = workflow.jobs.publish.steps.filter(item => item.name !== name);
    } else if (name === 'Checksum all release assets') {
      step.run = step.run.replace('sha256sum gpu-workload-supervisor*.deb >> SHA256SUMS', 'true');
    } else {
      step.with['subject-path'] = step.with['subject-path'].replace(/.*\.deb\n/g, '');
    }
    candidate[path] = YAML.stringify(workflow);
    assert.ok(inspect(candidate).some(error => error.includes(name)), name);
  }
});

test('desktop staging cannot omit an architecture, package validation, or the copy', () => {
  for (const [before, after] of [
    ['amd64 arm64', 'amd64'],
    ['bash scripts/check-desktop-package.sh "dist/$artifact"', 'true'],
    ['cp "dist/$artifact" "release-assets/$artifact"', 'true'],
  ]) {
    const candidate = files();
    const path = '.github/workflows/release.yml';
    const workflow = YAML.parse(candidate[path]);
    const step = workflow.jobs.publish.steps.find(item => item.name === 'Build desktop packages');
    assert.ok(step.run.includes(before));
    step.run = step.run.replace(before, after);
    candidate[path] = YAML.stringify(workflow);
    assert.ok(inspect(candidate).some(error => error.includes('Build desktop packages')));
  }
});

test('release checksums preserve the trusted baseline and append desktop packages', () => {
  const source = files();
  const path = '.github/workflows/release.yml';
  const workflow = YAML.parse(source[path]);
  const step = workflow.jobs.publish.steps.find(item => item.name === 'Checksum all release assets');
  const required = [
    'sha256sum gpu-workload-supervisor*.tar.gz sbom.cdx.json > SHA256SUMS',
    'sha256sum gpu-workload-supervisor*.deb >> SHA256SUMS',
    'sha256sum --check SHA256SUMS',
  ];
  for (const command of required) assert.ok(step.run.includes(command), command);
  for (const command of required) {
    const candidate = { ...source };
    const altered = structuredClone(workflow);
    const checksum = altered.jobs.publish.steps.find(item => item.name === step.name);
    checksum.run = checksum.run.replace(command, 'true');
    candidate[path] = YAML.stringify(altered);
    assert.ok(inspect(candidate).some(error => error.includes(step.name)), command);
  }
});

test('desktop integration gates cannot be removed, conditional, or allowed to fail', () => {
  for (const name of ['Native desktop transport integration', 'Packaged setup lifecycle integration', 'Debian payload lifecycle integration']) {
    for (const mutation of ['missing', 'conditional', 'allowed-failure', 'command']) {
      const candidate = files();
      const path = '.github/workflows/ci.yml';
      const workflow = YAML.parse(candidate[path]);
      const steps = workflow.jobs.checks.steps;
      const index = steps.findIndex(step => step.name === name);
      if (mutation === 'missing') steps.splice(index, 1);
      if (mutation === 'conditional') steps[index].if = false;
      if (mutation === 'allowed-failure') steps[index]['continue-on-error'] = true;
      if (mutation === 'command') steps[index].run = 'echo skipped';
      candidate[path] = YAML.stringify(workflow);
      assert.ok(inspect(candidate).some(error => error.includes(mutation === 'allowed-failure' ? 'may ignore step failures' : name)), `${name}: ${mutation}`);
    }
  }
});


test('dependency review cannot override the protected vulnerability configuration', () => {
  for (const override of [{ 'vulnerability-check': false }, { 'warn-only': true }, { 'config-file': './unchecked.yml' }]) {
    const candidate = files();
    const path = '.github/workflows/dependency-review.yml';
    const workflow = YAML.parse(candidate[path]);
    Object.assign(workflow.jobs.review.steps.find(step => step.name === 'Dependency review').with, override);
    candidate[path] = YAML.stringify(workflow);
    assert.ok(inspect(candidate).some(error => error.includes('without overrides')));
  }
});

test('ordinary immutable action pin refreshes pass across all workflows and composite setup', () => {
  const candidate = files({ serial: false });
  let count = 0;
  for (const path of Object.keys(candidate).filter(path => path.startsWith('.github/workflows/'))) {
    const workflow = YAML.parse(candidate[path]);
    for (const job of Object.values(workflow.jobs || {})) {
      for (const step of job.steps || []) {
        if (typeof step.uses === 'string' && /@[a-f0-9]{40}$/.test(step.uses)) {
          step.uses = step.uses.replace(/@[a-f0-9]{40}$/, '@' + 'a'.repeat(40));
          count++;
        }
      }
    }
    candidate[path] = YAML.stringify(workflow);
  }
  const actionPath = '.github/actions/setup-goreleaser/action.yml';
  const action = YAML.parse(candidate[actionPath]);
  action.runs.steps[0].uses = 'sigstore/cosign-installer@' + 'b'.repeat(40);
  candidate[actionPath] = YAML.stringify(action);
  assert.ok(count > 40, 'refresh exercises the complete protected action inventory');
  assert.deepEqual(inspectActivated(candidate), []);
});

test('every action-aware validator retains identity, immutable pins and execution controls', () => {
  const targets = [
    ['.github/workflows/main.yml', workflow => workflow.jobs.sonar.steps.find(step => step.uses?.startsWith('actions/download-artifact@'))],
    ['.github/workflows/ci.yml', workflow => workflow.jobs.go.steps.find(step => step.uses?.startsWith('actions/setup-node@'))],
    ['.github/workflows/pr-validation.yml', workflow => workflow.jobs.classify.steps[0]],
    ['.github/workflows/release.yml', workflow => workflow.jobs.validation.steps[0]],
    ['.github/actions/setup-goreleaser/action.yml', action => action.runs.steps[0]],
  ];
  const mutations = [
    step => { step.uses = 'attacker/replacement@' + 'a'.repeat(40); },
    step => { step.uses = step.uses.replace(/@.*/, '@v4'); },
    step => { step.uses = step.uses.replace(/@.*/, '@abc1234'); },
    step => { step.uses = step.uses.replace(/@.*/, '@<immutable-sha>'); },
    step => { step.env = { BASH_ENV: '/tmp/bypass' }; },
    step => { step.if = false; },
    step => { step.with = { ...step.with, 'unreviewed-input': true }; },
    step => { step.run = 'true'; },
    step => { step['continue-on-error'] = true; },
  ];
  for (const [path, select] of targets) {
    for (const mutate of mutations) {
      const candidate = files({ serial: false });
      const workflow = YAML.parse(candidate[path]);
      mutate(select(workflow));
      candidate[path] = YAML.stringify(workflow);
      assert.notDeepEqual(inspectActivated(candidate), [], `${path}: ${mutate}`);
    }
  }
  const candidate = files({ serial: false });
  delete candidate['.github/scripts/policy-action-pins.cjs'];
  assert.ok(inspectActivated(candidate).some(error => error.includes('Missing required file: .github/scripts/policy-action-pins.cjs')));
});

test('composite setup retains step order, commands and rejects ambiguous YAML', () => {
  const path = '.github/actions/setup-goreleaser/action.yml';
  for (const mutate of [
    action => { action.runs.steps[0].uses = { action: 'sigstore/cosign-installer', immutable: true }; },
    action => { action.runs.steps.reverse(); },
    action => { action.runs.steps[1].run += ' || true'; },
    action => { action.runs.steps[1].shell = 'sh'; },
    action => { action.runs.steps[0].with['cosign-release'] = 'latest'; },
    action => { action.runs.steps.push({ shell: 'bash', run: 'true' }); },
  ]) {
    const candidate = files({ serial: false });
    const action = YAML.parse(candidate[path]);
    mutate(action);
    candidate[path] = YAML.stringify(action);
    assert.ok(inspectActivated(candidate).some(error => error.includes('changed verified GoReleaser installer')));
  }
  const candidate = files({ serial: false });
  candidate[path] += '\nruns: {}\n';
  assert.ok(inspectActivated(candidate).some(error => error.includes('changed verified GoReleaser installer')));
});

test('Dependabot contracts permit comments, logging and renewed immutable action pins', () => {
  const candidate = files({ serial: false });
  const source = '.github/scripts/dependabot-auto-merge.cjs';
  candidate[source] = '// Reviewed explanatory comment\n' + candidate[source].replace(
    'const repository =', "core.info('Starting evaluation');\n  const repository =");
  for (const name of ['dependabot-auto-merge', 'dependabot-auto-merge-refresh']) {
    const path = `.github/workflows/${name}.yml`;
    candidate[path] = candidate[path].replaceAll(/@[a-f0-9]{40}/g, '@' + 'a'.repeat(40));
  }
  candidate['.github/workflows/dependabot-auto-merge.yml'] =
    candidate['.github/workflows/dependabot-auto-merge.yml'].replace(
      '25dd0e34f4fe68f24cc83900b1fe3fe149efef98', 'b'.repeat(40));
  assert.deepEqual(inspectActivated(candidate), []);
});

test('Dependabot queue, trusted inputs and refresh controls cannot be bypassed', () => {
  for (const mutate of [
    w => { w.jobs['enable-auto-merge'].steps.at(-1).with.script = 'return;'; },
    w => { w.jobs['enable-auto-merge'].steps.at(-1).if = false; },
    w => { w.jobs['enable-auto-merge'].steps.at(-1).env.GH_TOKEN = ''; },
    w => { w.jobs['enable-auto-merge'].steps[1].with['persist-credentials'] = true; },
    w => { w.jobs['enable-auto-merge'].steps[2].with.ref = 'main'; },
    w => { w.jobs['enable-auto-merge'].steps[2].with.repository = 'other/metadata'; },
    w => { w.concurrency['cancel-in-progress'] = true; },
    w => { delete w.permissions.actions; },
    w => { w.jobs['enable-auto-merge'].permissions = { contents: 'read' }; },
    w => { w.jobs['enable-auto-merge'].steps.at(-1).with.script += '\nconsole.log(\"extra queue code\");'; },
  ]) {
    const candidate = files({ serial: false });
    const path = '.github/workflows/dependabot-auto-merge.yml';
    const workflow = YAML.parse(candidate[path]); mutate(workflow);
    candidate[path] = YAML.stringify(workflow);
    assert.notDeepEqual(inspectActivated(candidate), []);
  }
  for (const mutate of [w => { w.jobs.refresh.if = false; },
    w => { delete w.on.schedule; }, w => { w.on.schedule = []; },
    w => { w.jobs.refresh.uses = './.github/workflows/main.yml'; }]) {
    const candidate = files({ serial: false });
    const path = '.github/workflows/dependabot-auto-merge-refresh.yml';
    const workflow = YAML.parse(candidate[path]); mutate(workflow);
    candidate[path] = YAML.stringify(workflow);
    assert.notDeepEqual(inspectActivated(candidate), []);
  }
});

test('selected Dependabot source safeguard removals fail without whole-file registration', () => {
  for (const [before, after] of [
    ['const MINIMUM_SCORE = 75;', 'const MINIMUM_SCORE = 0;'],
    ['const dependencies = metadata(pull, repository, metadataPath);', 'const dependencies = [];'],
    ['if (!eligible(commits, dependencies)) {', 'if (false) {'],
    ["['--disable-auto']", "['--auto']"],
    ['current.head.sha !== pull.head.sha', 'false'],
    ["'--match-head-commit', pull.head.sha", "'--match-head-commit', current.head.sha"],
    ['await ensureMainRun({ github, context, core, branch, mainWorkflow });', ''],
  ]) {
    const candidate = files({ serial: false });
    const path = '.github/scripts/dependabot-auto-merge.cjs';
    assert.ok(candidate[path].includes(before));
    candidate[path] = candidate[path].replace(before, after);
    assert.ok(inspectActivated(candidate).some(message => message.includes('Dependabot source lost safety contract')));
  }
  const candidate = files({ serial: false });
  candidate['.github/scripts/dependabot-auto-merge.cjs'] = 'module.exports = { run: async () => {} };';
  assert.notDeepEqual(inspectActivated(candidate), []);
});

function authorFixture({ association = 'OWNER', bot = false, changed = true } = {}) {
  const context = { repo: { owner: 'owner', repo: 'repo' }, payload: { pull_request: {
    number: 1, base: { sha: 'a'.repeat(40) }, head: { sha: 'b'.repeat(40) },
  } } };
  const reads = [];
  const github = { rest: {
    repos: { getContent: async params => {
      reads.push(params);
      return { data: { type: 'file', sha: (changed && params.ref === context.payload.pull_request.head.sha ? 'd' : 'c').repeat(40) } };
    } },
    pulls: { get: async () => ({ data: {
      author_association: association, user: { login: bot ? 'dependabot[bot]' : 'maintainer', type: bot ? 'Bot' : 'User' },
    } }) },
  } };
  return { github, context, reads };
}

test('automation source changes require a trusted human at immutable event refs', async () => {
  const { verifyDependabotAuthor } = require('./policy-guard.cjs');
  for (const association of ['OWNER', 'MEMBER', 'COLLABORATOR', 'CONTRIBUTOR', 'NONE']) {
    for (const bot of [false, true]) {
      const { github, context, reads } = authorFixture({ association, bot });
      if (!bot && ['OWNER', 'MEMBER', 'COLLABORATOR'].includes(association)) {
        await verifyDependabotAuthor(github, context);
      } else await assert.rejects(verifyDependabotAuthor(github, context), /trusted human author/);
      assert.deepEqual(reads.map(read => read.ref), Array.from({ length: 8 }, () => ['a'.repeat(40), 'b'.repeat(40)]).flat());
    }
  }
  const { github, context } = authorFixture({ changed: false });
  github.rest.pulls.get = async () => { throw new Error('No source author restriction for action pin updates'); };
  await verifyDependabotAuthor(github, context);
});

test('Dependabot configuration must retain active coverage of every required ecosystem and directory', () => {
  for (const mutate of [config => { config.updates = []; },
    config => { config.updates.pop(); },
    config => { config.updates[0]['open-pull-requests-limit'] = 0; },
    config => { delete config.updates[0].schedule; },
    config => { for (const entry of config.updates) entry['target-branch'] = 'maintenance'; },
    config => { for (const entry of config.updates) entry['exclude-paths'] = ['**']; },
    config => { for (const entry of config.updates) entry['pull-request-branch-name'] = { separator: '-' }; },
    config => { for (const entry of config.updates) entry['pull-request-branch-name'] = { separator: '/', prefix: 'deps' }; },
    config => { for (const entry of config.updates) entry['pull-request-branch-name'] = { separator: '/', template: 'deps/update' }; },
    config => { for (const entry of config.updates) entry.allow = [{ 'dependency-name': 'unused-dependency' }]; }]) {
    const candidate = files({ serial: false });
    const path = '.github/dependabot.yml';
    const config = YAML.parse(candidate[path]); mutate(config);
    candidate[path] = YAML.stringify(config);
    assert.ok(inspectActivated(candidate).includes('Dependabot lost required update coverage'));
  }
});


test('Dependabot retains PR reevaluation and merge recovery activities', () => {
  const required = ['opened', 'synchronize', 'reopened', 'ready_for_review', 'closed'];
  for (const types of [undefined, null, 'opened', {}, [], ['opened'], ['closed'],
    [...required, 'auto_merge_enabled'], [...required, 'auto_merge_disabled'],
    [...required, 'opened'], ['opened', 'opened', 'reopened', 'ready_for_review', 'closed'],
    [...required.slice(0, -1), null], [...required.slice(0, -1), {}],
    ...required.map(removed => required.filter(activity => activity !== removed))]) {
    const candidate = files({ serial: false });
    const path = '.github/workflows/dependabot-auto-merge.yml';
    const workflow = YAML.parse(candidate[path]);
    workflow.on.pull_request_target.types = types;
    candidate[path] = YAML.stringify(workflow);
    assert.ok(inspectActivated(candidate).includes('Dependabot lost required PR activities'), String(types));
  }
  const candidate = files({ serial: false });
  const path = '.github/workflows/dependabot-auto-merge.yml';
  const workflow = YAML.parse(candidate[path]);
  workflow.on.pull_request_target.types = required.toReversed();
  candidate[path] = YAML.stringify(workflow);
  assert.deepEqual(inspectActivated(candidate), [], 'intended activity order may change');
});

test('Dependabot cannot queue before either required checkout', () => {
  for (const name of ['Read trusted automation', 'Read pinned Dependabot metadata action']) {
    const candidate = files({ serial: false });
    const path = '.github/workflows/dependabot-auto-merge.yml';
    const workflow = YAML.parse(candidate[path]);
    const steps = workflow.jobs['enable-auto-merge'].steps;
    const queue = steps.pop();
    steps.splice(steps.findIndex(step => step.name === name), 0, queue);
    candidate[path] = YAML.stringify(workflow);
    assert.ok(inspectActivated(candidate).includes(`Dependabot checkout must precede queue: ${name}`));
  }
});


test('privileged Dependabot workflow rejects workspace replacement and execution overrides', () => {
  for (const mutate of [
    w => { w.jobs['enable-auto-merge'].steps.splice(3, 0, {
      uses: 'actions/checkout@' + 'a'.repeat(40), with: { repository: 'attacker/repo', path: 'automation' },
    }); },
    w => { w.jobs['enable-auto-merge'].steps.splice(3, 0, { run: 'echo overwritten > automation/.github/scripts/dependabot-auto-merge.cjs' }); },
    w => { w.jobs['enable-auto-merge'].steps[1].with.repository = 'attacker/repo'; },
    w => { w.jobs['enable-auto-merge'].steps[1].with.path = 'untrusted'; },
    w => { w.jobs['enable-auto-merge'].steps[1].env = { NODE_OPTIONS: '--require /tmp/override.cjs' }; },
    w => { w.jobs['enable-auto-merge'].steps.at(-1).env.NODE_OPTIONS = '--require /tmp/override.cjs'; },
    w => { w.jobs['enable-auto-merge'].steps.at(-1).with['github-token'] = '${{ secrets.OTHER_TOKEN }}'; },
    w => { w.env = { NODE_OPTIONS: '--require /tmp/override.cjs' }; },
    w => { w.jobs['enable-auto-merge'].env = { NODE_OPTIONS: '--require /tmp/override.cjs' }; },
    w => { w.defaults = { run: { 'working-directory': 'untrusted' } }; },
    w => { w.jobs['enable-auto-merge'].defaults = { run: { 'working-directory': 'untrusted' } }; },
    w => { w.jobs.extra = { 'runs-on': 'ubuntu-latest', steps: [{ run: 'true' }] }; },
    w => { w.on.pull_request_target.branches = ['unused-branch']; },
    w => { w.on.pull_request_target['branches-ignore'] = ['main']; },
    w => { w.on.pull_request_target.paths = ['unused/**']; },
  ]) {
    const candidate = files({ serial: false });
    const path = '.github/workflows/dependabot-auto-merge.yml';
    const workflow = YAML.parse(candidate[path]); mutate(workflow);
    candidate[path] = YAML.stringify(workflow);
    assert.notDeepEqual(inspectActivated(candidate), [], String(mutate));
  }
});

test('Dependabot ignores cannot disable required patch or minor updates', () => {
  for (const ignore of [{ 'dependency-name': '*' },
    { 'dependency-name': 'aislop' },
    { 'dependency-name': 'aislop', 'update-types': ['version-update:semver-patch'] },
    { 'dependency-name': 'aislop', 'update-types': ['version-update:semver-minor'] },
    { 'dependency-name': 'actions/checkout', versions: ['>= 0'], 'update-types': ['version-update:semver-major'] },
    ...[undefined, null, 0, '', '   ', [], {}].map(name => ({ 'dependency-name': name, 'update-types': ['version-update:semver-major'] })),
    null, [],
    { 'dependency-name': '**' }, { 'dependency-name': '***' },
    { 'dependency-name': '*/*', 'update-types': ['version-update:semver-patch', 'version-update:semver-minor'] },
    { 'dependency-name': 'actions/*', 'update-types': ['version-update:semver-minor'] },
    { 'dependency-name': '*', 'update-types': ['version-update:semver-minor'] },
    { 'dependency-name': '*', 'update-types': ['version-update:semver-patch'] }]) {
    const candidate = files({ serial: false });
    const path = '.github/dependabot.yml';
    const config = YAML.parse(candidate[path]);
    for (const entry of config.updates) entry.ignore = [ignore];
    candidate[path] = YAML.stringify(config);
    assert.ok(inspectActivated(candidate).includes('Dependabot lost required update coverage'));
  }
  for (const ignore of [
    { 'dependency-name': '*', 'update-types': ['version-update:semver-major'] },
    { 'dependency-name': '*/*', 'update-types': ['version-update:semver-major'] },
    { 'dependency-name': '*', versions: [], 'update-types': ['version-update:semver-major'] },
    { 'dependency-name': 'actions/checkout', versions: [], 'update-types': ['version-update:semver-major'] },
    { 'dependency-name': 'aislop', 'update-types': ['version-update:semver-major'] },
  ]) {
    const candidate = files({ serial: false });
    const path = '.github/dependabot.yml';
    const config = YAML.parse(candidate[path]);
    for (const entry of config.updates) entry.ignore = [ignore];
    candidate[path] = YAML.stringify(config);
    assert.deepEqual(inspectActivated(candidate), []);
  }
});

test('reverted live PR files cannot exempt a protected change at the inspected event head', async () => {
  const { run } = require('./policy-guard.cjs');
  const { github, context } = authorFixture({ association: 'NONE' });
  // The live PR has reverted to B; the queued event still inspects A.
  github.paginate = async () => [{ filename: 'README.md' }];
  github.rest.pulls.get = async () => ({ data: { head: { sha: 'reverted-live-head' },
    author_association: 'NONE', user: { login: 'outsider', type: 'User' } } });
  await assert.rejects(run({ github, context }), /trusted human author/);
});

test('protected source identity reads fail closed and detect deletion or rename', async () => {
  const { verifyDependabotAuthor } = require('./policy-guard.cjs');
  for (const failure of ['deleted', 'invalid', 'unavailable']) {
    const { github, context } = authorFixture({ association: 'NONE' });
    github.rest.repos.getContent = async params => {
      if (params.ref === context.payload.pull_request.head.sha) {
        if (failure === 'invalid') return { data: {} };
        const error = new Error(failure); error.status = failure === 'deleted' ? 404 : 500; throw error;
      }
      return { data: { type: 'file', sha: 'c'.repeat(40) } };
    };
    await assert.rejects(verifyDependabotAuthor(github, context),
      failure === 'deleted' ? /trusted human author/ : failure === 'invalid' ? /Cannot identify/ : /unavailable/);
  }
});


test('immutable source author check covers policy modules and rejects mutable or non-file identities', async () => {
  const { verifyDependabotAuthor } = require('./policy-guard.cjs');
  const { github, context } = authorFixture({ association: 'NONE', changed: false });
  const read = github.rest.repos.getContent;
  for (const name of ['policy-guard.cjs', 'policy-dependabot.cjs', 'policy-action-pins.cjs',
    'policy-release.cjs', 'policy-ci-parallel.cjs', 'policy-pr-scope.cjs', 'policy-ci-reuse.cjs']) {
    github.rest.repos.getContent = async params => params.path.endsWith('/' + name) &&
      params.ref === context.payload.pull_request.head.sha ?
      { data: { type: 'file', sha: 'd'.repeat(40) } } : read(params);
    await assert.rejects(verifyDependabotAuthor(github, context), /trusted human author/);
  }
  github.rest.repos.getContent = async () => ({ data: { type: 'symlink', sha: 'a'.repeat(40) } });
  await assert.rejects(verifyDependabotAuthor(github, context), /Cannot identify/);
  context.payload.pull_request.head.sha = 'main';
  await assert.rejects(verifyDependabotAuthor(github, context), /Immutable PR source refs/);
});


test('Dependabot explicit default branch separator retains required coverage', () => {
  const candidate = files({ serial: false });
  const path = '.github/dependabot.yml';
  const config = YAML.parse(candidate[path]);
  for (const entry of config.updates) entry['pull-request-branch-name'] = { separator: '/' };
  candidate[path] = YAML.stringify(config);
  assert.deepEqual(inspectActivated(candidate), []);
});


test('Dependabot policy module remains a required protected file', () => {
  const candidate = files({ serial: false });
  delete candidate['.github/scripts/policy-dependabot.cjs'];
  assert.ok(inspectActivated(candidate).includes('Missing required file: .github/scripts/policy-dependabot.cjs'));
});


test('Dependabot open PR limits accept defaults or positive integers only', () => {
  for (const limit of [0, -1, null, '3', 1.5, undefined, 3]) {
    const candidate = files({ serial: false });
    const path = '.github/dependabot.yml';
    const config = YAML.parse(candidate[path]);
    for (const entry of config.updates) {
      if (limit === undefined) delete entry['open-pull-requests-limit'];
      else entry['open-pull-requests-limit'] = limit;
    }
    candidate[path] = YAML.stringify(config);
    if (limit === undefined || limit === 3) assert.deepEqual(inspectActivated(candidate), []);
    else assert.ok(inspectActivated(candidate).includes('Dependabot lost required update coverage'), String(limit));
  }
});


test('pin normalization helper additions, deletions and read errors cannot bypass source review', async () => {
  const { verifyDependabotAuthor } = require('./policy-guard.cjs');
  for (const change of ['added', 'deleted', 'unavailable']) {
    const { github, context } = authorFixture({ association: 'NONE', changed: false });
    const read = github.rest.repos.getContent;
    github.rest.repos.getContent = async params => {
      const missingRef = change === 'added' ? context.payload.pull_request.base.sha : context.payload.pull_request.head.sha;
      if (params.path.endsWith('/policy-action-pins.cjs') && params.ref === missingRef) {
        const error = new Error('helper read failed'); error.status = change === 'unavailable' ? 500 : 404; throw error;
      }
      return read(params);
    };
    await assert.rejects(verifyDependabotAuthor(github, context),
      change === 'unavailable' ? /helper read failed/ : /trusted human author/);
  }
});


test('ignore rules require explicit major-only types without version ranges', () => {
  const invalid = [
    ...[['>= 0'], null, '>= 0', {}].map(versions => ({
      versions, 'update-types': ['version-update:semver-major'],
    })),
    ...[[], null, 'version-update:semver-major', ['unknown'],
      ['version-update:semver-major', 'version-update:semver-patch']].map(types => ({ 'update-types': types })),
  ];
  for (const name of ['*', 'aislop']) {
    for (const fields of invalid) {
      const candidate = files({ serial: false });
      const path = '.github/dependabot.yml';
      const config = YAML.parse(candidate[path]);
      for (const entry of config.updates) entry.ignore = [{ 'dependency-name': name, ...fields }];
      candidate[path] = YAML.stringify(config);
      assert.ok(inspectActivated(candidate).includes('Dependabot lost required update coverage'), JSON.stringify(fields));
    }
  }
});


test('Dependabot schedules support only bounded daily cron or simple predefined intervals', () => {
  const candidate = files({ serial: false });
  assert.deepEqual(inspectActivated(candidate), [], 'real baseline schedules');
  const path = '.github/dependabot.yml';
  const schedules = [
    { interval: 'cron', cronjob: '0 0 * * *' }, { interval: 'cron', cronjob: '59 23 * * *' },
    ...['daily', 'weekly', 'monthly'].map(interval => ({ interval })),
  ];
  for (const schedule of schedules) {
    const config = YAML.parse(candidate[path]);
    for (const entry of config.updates) entry.schedule = schedule;
    const changed = { ...candidate, [path]: YAML.stringify(config) };
    assert.deepEqual(inspectActivated(changed), [], JSON.stringify(schedule));
  }
  for (const schedule of [null, [], {},
    ...['61 25 32 13 8', '60 0 * * *', '0 24 * * *', '-1 0 * * *',
      '0 0 1 * *', '*/5 * * * *', 'every day', 0, null, {}].map(cronjob => ({ interval: 'cron', cronjob })),
    { interval: 'daily', time: '25:00' }, { interval: 'weekly', day: 'unknown' },
    { interval: 'monthly', timezone: 'UTC' }, { interval: 'cron', cronjob: '0 0 * * *', timezone: 'UTC' },
  ]) {
    const config = YAML.parse(candidate[path]);
    for (const entry of config.updates) entry.schedule = schedule;
    const changed = { ...candidate, [path]: YAML.stringify(config) };
    assert.ok(inspectActivated(changed).includes('Dependabot lost required update coverage'), JSON.stringify(schedule));
  }
});

test('Dependabot recovery rejects unsupported, invalid or malformed daily cron schedules', () => {
  for (const cron of ['0 0 * * *', '59 23 * * *']) {
    const candidate = files({ serial: false });
    const path = '.github/workflows/dependabot-auto-merge-refresh.yml';
    const workflow = YAML.parse(candidate[path]); workflow.on.schedule = [{ cron }];
    candidate[path] = YAML.stringify(workflow);
    assert.deepEqual(inspectActivated(candidate), []);
  }
  for (const schedule of [[{ cron: '61 25 32 13 8' }], [{ cron: 0 }], [null], [[]], ['0 0 * * *'],
    [{ cron: '0 0 * * *', timezone: 'UTC' }], [{ cron: '0 0 * * *' }, { cron: 'invalid' }], {}]) {
    const candidate = files({ serial: false });
    const path = '.github/workflows/dependabot-auto-merge-refresh.yml';
    const workflow = YAML.parse(candidate[path]); workflow.on.schedule = schedule;
    candidate[path] = YAML.stringify(workflow);
    assert.ok(inspectActivated(candidate).includes('Dependabot lost scheduled recovery'));
  }
});


test('Dependabot scopes occur exactly once and cannot add unknown or overlapping directories', () => {
  const baseline = files({ serial: false });
  assert.deepEqual(inspectActivated(baseline), [], 'real five-scope baseline');
  const path = '.github/dependabot.yml';
  for (const mutate of [
    config => { config.updates.push(structuredClone(config.updates[0])); },
    config => { config.updates[1] = structuredClone(config.updates[0]); },
    config => { config.updates.push({ ...config.updates[1], directory: '/unused' }); },
    config => { config.updates[1].directory = '/unused'; },
    config => { config.updates[1].directory = '/**'; },
    config => { config.updates[1].directories = ['/.github/aislop']; },
    config => { config.updates[1].directories = ['/.github/aislop', '/.github/scripts']; },
    config => { delete config.updates[1].directory; config.updates[1].directories = ['/.github/aislop']; },
  ]) {
    const config = YAML.parse(baseline[path]); mutate(config);
    const candidate = { ...baseline, [path]: YAML.stringify(config) };
    assert.ok(inspectActivated(candidate).includes('Dependabot lost required update coverage'), String(mutate));
  }
});


test('Dependabot preserves automatic rebases with default or explicit auto strategy', () => {
  const baseline = files({ serial: false });
  assert.deepEqual(inspectActivated(baseline), [], 'current config uses auto');
  const path = '.github/dependabot.yml';
  for (const strategy of [undefined, 'auto', 'disabled', null, true, 0, 'AUTO', {}, []]) {
    const config = YAML.parse(baseline[path]);
    for (const entry of config.updates) {
      if (strategy === undefined) delete entry['rebase-strategy'];
      else entry['rebase-strategy'] = strategy;
    }
    const candidate = { ...baseline, [path]: YAML.stringify(config) };
    if (strategy === undefined || strategy === 'auto') assert.deepEqual(inspectActivated(candidate), []);
    else assert.ok(inspectActivated(candidate).includes('Dependabot lost required update coverage'), JSON.stringify(strategy));
  }
});


test('each npm scope retains manifest-capable versioning and other ecosystems keep defaults', () => {
  const baseline = files({ serial: false });
  assert.deepEqual(inspectActivated(baseline), [], 'current default strategies');
  const path = '.github/dependabot.yml';
  const original = YAML.parse(baseline[path]);
  const supported = ['auto', 'increase', 'increase-if-necessary', 'widen'];
  for (const [index, entry] of original.updates.entries()) {
    for (const strategy of [undefined, ...supported, 'lockfile-only', null, 0, {}, ['auto']]) {
      const config = structuredClone(original);
      if (strategy !== undefined) config.updates[index]['versioning-strategy'] = strategy;
      const candidate = { ...baseline, [path]: YAML.stringify(config) };
      const accepted = strategy === undefined ||
        (entry['package-ecosystem'] === 'npm' && supported.includes(strategy));
      if (accepted) assert.deepEqual(inspectActivated(candidate), [], `${entry.directory}: ${strategy}`);
      else assert.ok(inspectActivated(candidate).includes('Dependabot lost required update coverage'),
        `${entry['package-ecosystem']}:${entry.directory}: ${JSON.stringify(strategy)}`);
    }
  }
});


test('Dependabot scopes remain independent of multi-ecosystem inheritance', () => {
  const baseline = files({ serial: false });
  const path = '.github/dependabot.yml';
  const original = YAML.parse(baseline[path]);
  assert.ok(original.updates.find(entry => entry['package-ecosystem'] === 'github-actions').groups.codeql);
  assert.deepEqual(inspectActivated(baseline), [], 'ordinary CodeQL group remains supported');
  for (const mutate of [
    config => { config['multi-ecosystem-groups'] = { combined: { schedule: { interval: 'daily' } } }; },
    config => { config['multi-ecosystem-groups'] = {}; },
    config => { config['multi-ecosystem-groups'] = null; },
    config => { config.updates[0]['multi-ecosystem-group'] = 'combined'; },
    config => { config.updates[0]['multi-ecosystem-group'] = null; },
  ]) {
    const config = structuredClone(original); mutate(config);
    const candidate = { ...baseline, [path]: YAML.stringify(config) };
    assert.ok(inspectActivated(candidate).includes('Dependabot lost required update coverage'));
  }
});


test('Dependabot cooldown supports only absent or integer default-days from 1 through 90', () => {
  const baseline = files({ serial: false });
  assert.deepEqual(inspectActivated(baseline), [], 'current cooldowns');
  const path = '.github/dependabot.yml';
  for (const cooldown of [undefined, { 'default-days': 1 }, { 'default-days': 90 },
    null, 7, '7', [], {}, ...[0, -1, 91, 1.5, '7', null].map(days => ({ 'default-days': days })),
    { 'default-days': 7, 'semver-minor-days': 1 }, { 'default-days': 7, include: ['*'] },
  ]) {
    const config = YAML.parse(baseline[path]);
    for (const entry of config.updates) {
      if (cooldown === undefined) delete entry.cooldown;
      else entry.cooldown = cooldown;
    }
    const candidate = { ...baseline, [path]: YAML.stringify(config) };
    if (cooldown === undefined || cooldown?.['default-days'] === 1 || cooldown?.['default-days'] === 90) {
      assert.deepEqual(inspectActivated(candidate), []);
    } else assert.ok(inspectActivated(candidate).includes('Dependabot lost required update coverage'), JSON.stringify(cooldown));
  }
});

test('Dependabot supports only known top-level and update-entry options', () => {
  const baseline = files({ serial: false });
  const path = '.github/dependabot.yml';
  for (const mutate of [
    config => { config.registries = {}; }, config => { config.unknown = true; },
    config => { config.updates[0].unknown = true; },
    config => { config.updates[0].ignore = null; },
    config => { config.updates[0].ignore = { 'dependency-name': '*' }; },
    ...[null, [], '/', {}, { separator: '/', extra: true }].map(name => config => { config.updates[0]['pull-request-branch-name'] = name; }),
  ]) {
    const config = YAML.parse(baseline[path]); mutate(config);
    assert.ok(inspectActivated({ ...baseline, [path]: YAML.stringify(config) }).includes('Dependabot lost required update coverage'));
  }
});

test('Dependabot permits absent groups or the exact CodeQL group with either pattern order', () => {
  const baseline = files({ serial: false });
  const path = '.github/dependabot.yml';
  const original = YAML.parse(baseline[path]);
  const index = original.updates.findIndex(entry => entry['package-ecosystem'] === 'github-actions');
  for (const groups of [undefined, { codeql: { patterns: ['github/codeql-action/*', 'github/codeql-action'] } }]) {
    const config = structuredClone(original);
    if (groups === undefined) delete config.updates[index].groups;
    else config.updates[index].groups = groups;
    assert.deepEqual(inspectActivated({ ...baseline, [path]: YAML.stringify(config) }), []);
  }
  for (const groups of [null, [], {}, 'codeql', { other: { patterns: ['*'] } },
    { codeql: null }, { codeql: { patterns: ['github/codeql-action'] } },
    { codeql: { patterns: ['github/codeql-action', 'github/codeql-action'] } },
    { codeql: { patterns: ['github/codeql-action', '*'] } },
    { codeql: { patterns: ['github/codeql-action', 'github/codeql-action/*'], 'update-types': ['major'] } },
    { codeql: { patterns: ['github/codeql-action', 'github/codeql-action/*'] }, other: {} },
  ]) {
    const config = structuredClone(original); config.updates[index].groups = groups;
    assert.ok(inspectActivated({ ...baseline, [path]: YAML.stringify(config) }).includes('Dependabot lost required update coverage'), JSON.stringify(groups));
  }
  for (const [position, entry] of original.updates.entries()) {
    if (entry['package-ecosystem'] === 'github-actions') continue;
    const config = structuredClone(original); config.updates[position].groups = original.updates[index].groups;
    assert.ok(inspectActivated({ ...baseline, [path]: YAML.stringify(config) }).includes('Dependabot lost required update coverage'));
  }
});

test('Dependabot concurrency accepts only its supported two-key serialization object', () => {
  const baseline = files({ serial: false });
  assert.deepEqual(inspectActivated(baseline), []);
  const path = '.github/workflows/dependabot-auto-merge.yml';
  const original = YAML.parse(baseline[path]);
  for (const concurrency of [
    { ...original.concurrency, bogus: true }, null, [], 'shared-group', false,
    { group: original.concurrency.group }, { 'cancel-in-progress': false },
    { ...original.concurrency, 'cancel-in-progress': 'false' },
  ]) {
    const workflow = structuredClone(original); workflow.concurrency = concurrency;
    const candidate = { ...baseline, [path]: YAML.stringify(workflow) };
    assert.ok(inspectActivated(candidate).includes('Dependabot lost gated queue invocation or serialization'),
      JSON.stringify(concurrency));
  }
  const workflow = structuredClone(original);
  workflow.concurrency = { 'cancel-in-progress': false, group: original.concurrency.group };
  assert.deepEqual(inspectActivated({ ...baseline, [path]: YAML.stringify(workflow) }), []);
});

test('Dependabot workflow and job names allow omission or string labels only', () => {
  for (const [file, job] of [['dependabot-auto-merge', 'enable-auto-merge'],
    ['dependabot-auto-merge-refresh', 'refresh']]) {
    const baseline = files({ serial: false });
    const path = `.github/workflows/${file}.yml`;
    const original = YAML.parse(baseline[path]);
    for (const target of ['workflow', 'job']) {
      for (const name of [null, 0, true, {}, []]) {
        const workflow = structuredClone(original);
        (target === 'workflow' ? workflow : workflow.jobs[job]).name = name;
        const candidate = { ...baseline, [path]: YAML.stringify(workflow) };
        assert.ok(inspectActivated(candidate).includes('Dependabot workflow and job names must be strings'),
          `${file}/${target}: ${JSON.stringify(name)}`);
      }
    }
    for (const name of [undefined, 'Reviewed display label']) {
      const workflow = structuredClone(original);
      if (name === undefined) { delete workflow.name; delete workflow.jobs[job].name; }
      else { workflow.name = name; workflow.jobs[job].name = name; }
      assert.deepEqual(inspectActivated({ ...baseline, [path]: YAML.stringify(workflow) }), []);
    }
  }
});
