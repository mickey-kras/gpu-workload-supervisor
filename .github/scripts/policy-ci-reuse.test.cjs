const test = require('node:test');
const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const { execFileSync } = require('node:child_process');
const { normalizeCiReuse } = require('./policy-ci-reuse.cjs');
const { reuseFixture } = require('./ci-reuse-fixture.cjs');

const CI = '.github/workflows/ci.yml';
const AISLOP = '.github/workflows/aislop.yml';
const MAIN = '.github/workflows/main.yml';
function sources() {
  return Object.fromEntries(['quality-coverage.py', 'check-scan-reports.py']
    .map(name => [`.github/scripts/${name}`, readFileSync(`.github/scripts/${name}`, 'utf8')]));
}
function errors(workflows, files = sources()) {
  const failures = [];
  normalizeCiReuse(files, workflows, failures);
  return failures;
}
function mutate(fn) {
  const workflows = reuseFixture();
  fn(workflows);
  assert.notDeepEqual(errors(workflows), []);
}
function step(workflows, path, job, name) {
  return workflows[path].jobs[job].steps.find(step => step.name === name);
}

test('independent reuse fixture satisfies the reviewed scanner and coverage profile', () => {
  assert.deepEqual(errors(reuseFixture()), []);
});

test('reporting scope, unfixed handling, vulnerability exit status and single scans stay protected', () => {
  mutate(workflows => { step(workflows, CI, 'scans', 'Trivy filesystem report').with.severity = 'HIGH,CRITICAL'; });
  mutate(workflows => { delete step(workflows, CI, 'scans', 'Trivy filesystem report').env; });
  mutate(workflows => { step(workflows, CI, 'scans', 'Trivy filesystem report').with['ignore-unfixed'] = false; });
  mutate(workflows => { step(workflows, CI, 'scans', 'Trivy high and critical gate').run = 'true'; });
  mutate(workflows => { step(workflows, CI, 'scans', 'Convert Trivy SARIF').run = 'true'; });
  mutate(workflows => { workflows[CI].jobs.scans.steps.push({ run: 'trivy fs .' }); });
  mutate(workflows => { step(workflows, AISLOP, 'status', 'Aislop Go quality gate').run += ' || true'; });
  mutate(workflows => { step(workflows, AISLOP, 'status', 'Aislop findings gate').run = 'true'; });
});

test('only successful producers can upload coverage and Sonar consumes exact artifact IDs', () => {
  mutate(workflows => { step(workflows, CI, 'go', 'Upload go quality coverage').if = '${{ always() }}'; });
  mutate(workflows => { step(workflows, CI, 'frontend', 'Upload javascript quality coverage').with['if-no-files-found'] = 'warn'; });
  mutate(workflows => { step(workflows, CI, 'go', 'Stage go quality coverage').run = 'touch coverage.out'; });
  mutate(workflows => { step(workflows, MAIN, 'sonar', 'Download Go quality coverage').with.name = 'latest-coverage'; });
  mutate(workflows => { step(workflows, MAIN, 'sonar', 'Download Go quality coverage').with['run-id'] = '123'; });
  mutate(workflows => { step(workflows, MAIN, 'sonar', 'Validate and restore quality coverage').run = 'true'; });
  mutate(workflows => { workflows[MAIN].jobs.sonar.steps.push({ run: 'touch coverage.out' }); });
  mutate(workflows => { workflows[MAIN].env = { BASH_ENV: '/tmp/skip' }; });
});

test('candidate helpers cannot weaken source identity, digest or scanner validation', () => {
  for (const path of Object.keys(sources())) {
    const files = sources();
    files[path] += '\n# changed validation\n';
    assert.notDeepEqual(errors(reuseFixture(), files), []);
    delete files[path];
    assert.notDeepEqual(errors(reuseFixture(), files), []);
  }
});

test('coverage and scanner helper behavior rejects missing, mismatched and unreadable evidence', () => {
  execFileSync('python3', ['-m', 'unittest', 'discover', '-s', 'tests', '-p', 'test_ci_reuse.py'], { stdio: 'pipe' });
});
