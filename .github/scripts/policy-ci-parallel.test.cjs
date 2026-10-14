const test = require('node:test');
const assert = require('node:assert/strict');
const { spawnSync } = require('node:child_process');
const { normalizeParallelCi, REQUIRED_JOBS } = require('./policy-ci-parallel.cjs');
const { parallelFixture } = require('./ci-parallel-fixture.cjs');

const path = '.github/workflows/ci.yml';
function errors(workflow) {
  const failures = [];
  normalizeParallelCi({ [path]: workflow }, failures);
  return failures;
}
function mutate(fn) {
  const workflow = parallelFixture();
  fn(workflow.jobs);
  assert.notDeepEqual(errors(workflow), []);
}

test('reviewed parallel graph preserves qualification ordering', () => {
  assert.deepEqual(errors(parallelFixture()), []);
});

test('every required job is unconditional and included in aggregate', () => {
  for (const id of REQUIRED_JOBS) {
    mutate(jobs => { delete jobs[id]; });
    mutate(jobs => { jobs[id].if = false; });
    mutate(jobs => { jobs[id].needs = 'checks'; });
    mutate(jobs => { jobs.checks.needs = jobs.checks.needs.filter(name => name !== id); });
  }
  mutate(jobs => { delete jobs.checks.if; });
  mutate(jobs => { jobs.checks.if = '${{ success() }}'; });
  mutate(jobs => { jobs.checks.name = 'new required check'; });
});

test('shard omission, duplicate shards, quick mode and evidence collisions fail', () => {
  mutate(jobs => { jobs.native.strategy.matrix.shard.pop(); });
  mutate(jobs => { jobs.native.strategy.matrix.shard[5] = 4; });
  mutate(jobs => { jobs.native.strategy['fail-fast'] = true; });
  mutate(jobs => { jobs.native.steps.find(step => step.name?.includes('matrix')).run += ' --quick'; });
  mutate(jobs => { jobs.native.steps.find(step => step.name === 'Upload native setup evidence').with.name = 'native-evidence'; });
  mutate(jobs => { delete jobs.native.steps.find(step => step.name === 'Upload native setup evidence').if; });
});

test('candidate upload follows all gates and consumes only current-run qualified packages', () => {
  mutate(jobs => { jobs.checks.steps.reverse(); });
  mutate(jobs => { jobs.package.steps.reverse(); });
  mutate(jobs => { delete jobs.package.outputs; });
  mutate(jobs => { jobs.checks.steps.find(step => step.name === 'Download qualified package payload').with = { name: 'payload-for-current-attempt', path: 'dist/' }; });
  mutate(jobs => { jobs.checks.steps.find(step => step.name === 'Download qualified package payload').with['run-id'] = '123'; });
  mutate(jobs => { jobs.checks.steps.find(step => step.name === 'Upload CI-tested candidate artifacts').if = '${{ always() }}'; });
  mutate(jobs => { jobs.checks.steps.find(step => step.name === 'Require every CI job to succeed').run = 'true'; });
  mutate(jobs => { jobs.go.steps.find(step => step.name === 'Real systemd lifecycle qualification').run = 'true'; });
  mutate(jobs => { jobs.package.steps.find(step => step.name === 'Verify snapshot desktop packages').run = 'true'; });
  mutate(jobs => { jobs.package.steps.find(step => step.uses?.startsWith('actions/checkout@')).with.ref = 'main'; });
});

test('actual aggregate command rejects failed, cancelled, skipped, absent and unknown work', () => {
  const workflow = parallelFixture();
  const command = workflow.jobs.checks.steps.find(step => step.name === 'Require every CI job to succeed').run;
  const success = Object.fromEntries(REQUIRED_JOBS.map(id => [id, { result: 'success' }]));
  success.package.outputs = { 'payload-id': '1234', 'qualified-attempt': '1' };
  const run = results => spawnSync('bash', ['-e', '-c', command], {
    env: { ...process.env, CI_NEEDS: JSON.stringify(results) }, encoding: 'utf8',
  });
  assert.equal(run(success).status, 0);
  for (const id of REQUIRED_JOBS) {
    for (const result of ['failure', 'cancelled', 'skipped', 'pending', undefined]) {
      const results = structuredClone(success);
      results[id] = { result };
      assert.notEqual(run(results).status, 0, `${id}: ${result}`);
    }
    const missing = structuredClone(success);
    delete missing[id];
    assert.notEqual(run(missing).status, 0);
  }
  assert.notEqual(run({ ...success, unknown: { result: 'success' } }).status, 0);
  assert.notEqual(run({}).status, 0);
  for (const outputs of [{}, { 'payload-id': '', 'qualified-attempt': '1' },
    { 'payload-id': '1234', 'qualified-attempt': '' }, { 'payload-id': 'invalid', 'qualified-attempt': '1' }]) {
    assert.notEqual(run({ ...success, package: { result: 'success', outputs } }).status, 0);
  }
});

test('unnamed scripts and action environment changes cannot alter protected gates', () => {
  mutate(jobs => { jobs.go.steps.splice(2, 0, { run: 'echo BASH_ENV=/tmp/skip >> "$GITHUB_ENV"' }); });
  mutate(jobs => { jobs.checks.steps.push({ run: 'echo replacement > candidate-artifacts/checksums.txt' }); });
  mutate(jobs => { jobs.scans.steps.splice(2, 0, { uses: 'actions/checkout@' + 'a'.repeat(40) }); });
  mutate(jobs => { jobs.go.steps.find(step => step.uses?.startsWith('actions/setup-go@')).env = { BASH_ENV: '/tmp/skip' }; });
  mutate(jobs => { jobs.go.steps.find(step => step.name === 'Go formatting').env = { BASH_ENV: '/tmp/skip' }; });
  mutate(jobs => { jobs.scans.steps.find(step => step.name === 'Trivy high and critical gate').with['scan-ref'] = 'empty'; });
});

test('workflow and job environments cannot bypass the explicit step controls', () => {
  const workflow = parallelFixture();
  workflow.env = { BASH_ENV: '/tmp/skip' };
  assert.notDeepEqual(errors(workflow), []);
  mutate(jobs => { jobs.go.env = { BASH_ENV: '/tmp/skip' }; });
  mutate(jobs => { jobs.go.steps.find(step => step.name === 'Go vulnerability audit').with['go-package'] = './empty'; });
  mutate(jobs => { jobs.package.steps.find(step => step.uses?.startsWith('actions/setup-node@')).with['node-version'] = 24; });
});

test('changing the guard shard command alone cannot silently reduce fixture coverage', () => {
  const { readFileSync } = require('node:fs');
  const { runInNewContext } = require('node:vm');
  const source = readFileSync('.github/scripts/policy-ci-parallel.cjs', 'utf8');
  const changed = source.replace("--shard-count 6';", "--shard-count 6 --quick';");
  assert.notEqual(changed, source);
  const module = { exports: {} };
  runInNewContext(changed, { module, require });
  const fixtureModule = { exports: {} };
  runInNewContext(readFileSync('.github/scripts/ci-parallel-fixture.cjs', 'utf8'), {
    module: fixtureModule,
    structuredClone,
    require: name => name === './policy-ci-parallel.cjs' ? module.exports : require(name),
  });
  const failures = [];
  module.exports.normalizeParallelCi({ [path]: fixtureModule.exports.parallelFixture() }, failures);
  assert.ok(failures.some(failure => failure.includes('GTK shard command')));
});
