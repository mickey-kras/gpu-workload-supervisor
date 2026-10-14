const test = require('node:test');
const assert = require('node:assert/strict');
const { readFileSync, readdirSync } = require('node:fs');
const { inspect: inspectActivated } = require('./policy-guard.cjs');
const { runInNewContext } = require('node:vm');
const parallelPolicy = require('./policy-ci-parallel.cjs');

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
  const source = result[path];
  if (serial && YAML.parse(source).jobs.native) {
    const jobsIndex = source.indexOf('jobs:\n');
    const sections = source.slice(jobsIndex + 6).split(/^  [a-z]+:\n/m).slice(1);
    const steps = sections.map(section => section.slice(section.indexOf('    steps:\n') + 11)).join('');
    result[path] = source.slice(0, jobsIndex) + 'jobs:\n  checks:\n    name: checks\n    runs-on: ubuntu-latest\n    steps:\n' + steps;
  }
  return result;
}

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

test('release validation policy accepts strict reuse alongside the retained legacy route', () => {
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

test('release verifier bytes are protected even before switching to reuse', () => {
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

test('legacy release gates cannot be weakened during staged validation adoption', () => {
  for (const mutate of [
    jobs => { jobs.quality.if = false; },
    jobs => { jobs.aislop.uses = './.github/workflows/ci.yml'; },
    jobs => { jobs.codeql.needs = []; },
    jobs => { jobs.publish.steps.find(step => step.name ===
      'Verify main and successful validation at this commit').with.script = 'return;'; },
  ]) {
    const candidate = files({ serial: false }); const path = '.github/workflows/release.yml';
    const workflow = YAML.parse(candidate[path]); mutate(workflow.jobs);
    candidate[path] = YAML.stringify(workflow);
    assert.notDeepEqual(inspect(candidate), []);
  }
});

test('legacy verification requires the pinned action and exact runtime inputs and setup', () => {
  for (const mutate of [
    (workflow, gate) => { gate.uses = 'actions/checkout@' + 'a'.repeat(40); },
    (workflow, gate) => { gate.uses = 'actions/github-script@' + 'a'.repeat(40); },
    (workflow, gate) => { gate.with['github-token'] = '${{ secrets.WRITE_TOKEN }}'; },
    (workflow, gate) => { gate.env.NODE_OPTIONS = '--require ./malicious.cjs'; },
    (workflow, gate) => { gate.env.VERSION = '0.1.0'; },
    workflow => { workflow.env = { NODE_OPTIONS: '--require ./malicious.cjs' }; },
    workflow => { workflow.jobs.publish.env = { NODE_OPTIONS: '--require ./malicious.cjs' }; },
    workflow => { workflow.jobs.publish['runs-on'] = 'self-hosted'; },
    workflow => { workflow.jobs.publish.steps.splice(4, 0, { run: 'overwrite runtime' }); },
    workflow => { workflow.jobs.publish.steps[1].with.repository = 'attacker/repo'; },
  ]) {
    const candidate = files({ serial: false }); const path = '.github/workflows/release.yml';
    const workflow = YAML.parse(candidate[path]); const gate = workflow.jobs.publish.steps.find(step =>
      step.name === 'Verify main and successful validation at this commit');
    mutate(workflow, gate); candidate[path] = YAML.stringify(workflow);
    assert.ok(inspect(candidate).some(error => error.includes('legacy exact-commit release validation')));
  }
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

test('verified GoReleaser route rejects changed installer or action bytes', () => {
  for (const path of ['.github/actions/setup-goreleaser/action.yml', '.github/scripts/install-goreleaser.sh']) {
    const candidate = files();
    candidate[path] += '\n# changed\n';
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
