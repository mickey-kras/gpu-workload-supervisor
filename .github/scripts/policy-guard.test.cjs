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
    'release-version.json', '.github/scripts/policy-release.cjs',
    '.github/scripts/package.json', '.github/scripts/package-lock.json',
    '.github/aislop/package.json', '.github/aislop/package-lock.json',
    '.github/dependency-review-config.yml', '.semgrep.yml', '.aislop/config.yml',
    'sonar-project.properties', '.goreleaser.yaml', '.testcoverage.yml',
    '.github/actions/setup-goreleaser/action.yml', '.github/scripts/install-goreleaser.sh',
  ]) result[path] = readFileSync(path, 'utf8');
  for (const name of ['package.json', 'package-lock.json', 'audit-ci.json', 'audit.cjs', 'audit.test.cjs', 'audit-fixture.json']) {
    try { result[`.github/audit-tool/${name}`] = readFileSync(`.github/audit-tool/${name}`, 'utf8'); } catch (error) { if (error.code !== 'ENOENT') throw error; }
  }
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

test('temporary audit exception rejects changed policy, tooling, and command', () => {
  for (const path of ['audit-ci.json', 'audit.cjs', 'package.json', 'package-lock.json']) {
    const candidate = temporaryAuditFiles();
    candidate[`.github/audit-tool/${path}`] += '\n';
    assert.ok(inspect(candidate).some(error => error.includes('temporary audit exception')));
  }
  const candidate = temporaryAuditFiles();
  candidate['.github/workflows/ci.yml'] = candidate['.github/workflows/ci.yml'].replace('node .github/audit-tool/audit.cjs .github/aislop', 'node .github/audit-tool/audit.cjs .github/aislop || true');
  assert.ok(inspect(candidate).some(error => error.includes('changed gate commands')));
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
