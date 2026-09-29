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
  for (const name of readdirSync('.github/rulesets')) {
    if (name.endsWith('.json')) {
      result[`.github/rulesets/${name}`] = readFileSync(`.github/rulesets/${name}`, 'utf8');
    }
  }
  for (const path of [
    '.github/dependabot.yml', '.github/scripts/policy-guard.cjs',
    '.github/scripts/dependabot-auto-merge.cjs', '.github/scripts/pr-branch-updater.cjs',
    '.github/scripts/package.json', '.github/scripts/package-lock.json',
    '.github/aislop/package.json', '.github/aislop/package-lock.json',
    '.github/dependency-review-config.yml', '.semgrep.yml', '.aislop/config.yml',
    'sonar-project.properties',
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

test('required status checks cannot be removed from importable ruleset', () => {
  const candidate = files();
  const path = '.github/rulesets/protect-default-branch.json';
  candidate[path] = candidate[path].replace('dependency-review / dependency review', 'disabled review');
  assert.ok(inspect(candidate).some(error => error.includes('Default branch lost required check')));
});

test('removed policy files and unpinned actions fail closed', () => {
  const errors = inspect({
    '.github/workflows/example.yml': 'jobs:\n  example:\n    steps:\n      - uses: actions/checkout@v7\n',
  });
  assert.ok(errors.some(error => error.includes('Missing required file')));
  assert.ok(errors.some(error => error.includes('unpin')));
});
