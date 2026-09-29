const test = require('node:test');
const assert = require('node:assert/strict');
const { readFileSync, readdirSync } = require('node:fs');
const { inspect } = require('./policy-guard.cjs');

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
    'sonar-project.properties', '.goreleaser.yaml',
  ]) result[path] = readFileSync(path, 'utf8');
  return result;
}

test('current governance workflows satisfy the trusted guard', () => {
  assert.deepEqual(inspect(files()), []);
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
