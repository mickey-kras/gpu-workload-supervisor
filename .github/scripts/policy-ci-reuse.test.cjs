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


