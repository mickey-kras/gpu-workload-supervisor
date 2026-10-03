const test = require('node:test');
const assert = require('node:assert/strict');
const { readFileSync, readdirSync } = require('node:fs');
const { inspect } = require('./policy-guard.cjs');
const YAML = require('yaml');

function files() {
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
    'release-version.json',
    '.github/scripts/package.json', '.github/scripts/package-lock.json',
    '.github/aislop/package.json', '.github/aislop/package-lock.json',
    '.github/dependency-review-config.yml', '.semgrep.yml', '.aislop/config.yml',
    'sonar-project.properties', '.goreleaser.yaml', '.testcoverage.yml',
    '.github/actions/setup-goreleaser/action.yml', '.github/scripts/install-goreleaser.sh',
  ]) result[path] = readFileSync(path, 'utf8');
  return result;
}

test('current governance workflows satisfy the trusted guard', () => {
  assert.deepEqual(inspect(files()), []);
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

test('Trivy report identity and successful report condition protect SARIF upload', () => {
  const path = '.github/workflows/ci.yml';
  for (const change of ['report-id', 'report-output', 'upload-condition', 'fork-condition']) {
    const candidate = files();
    const workflow = YAML.parse(candidate[path]);
    const report = workflow.jobs.checks.steps.find(step => step.name === 'Trivy filesystem report');
    const upload = workflow.jobs.checks.steps.find(step => step.name === 'Upload Trivy SARIF');
    if (change === 'report-id') report.id = 'wrong-report';
    if (change === 'report-output') report.with.output = 'wrong.sarif';
    if (change === 'upload-condition') upload.if = upload.if.replace("steps.trivy-report.outcome == 'success' && ", '');
    if (change === 'fork-condition') upload.if = "${{ !cancelled() && steps.trivy-report.outcome == 'success' }}";
    candidate[path] = YAML.stringify(workflow);
    assert.ok(inspect(candidate).some(error => error.includes('Trivy')), change);
  }
});
