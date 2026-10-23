const test = require('node:test');
const assert = require('node:assert/strict');
const { readFileSync, mkdtempSync, mkdirSync, writeFileSync, rmSync } = require('node:fs');
const { tmpdir } = require('node:os');
const { join } = require('node:path');
const { execFileSync } = require('node:child_process');
const { normalizeScopedValidation, FULL_IF, GO_STEPS, CANDIDATE_STEPS,
  SCOPE_INPUT, CLASSIFIER_JOB, SCOPED_QUALITY, SCOPED_AGGREGATE, AGGREGATE_ENV, CLASSIFIER_SCRIPT } = require('./policy-pr-scope.cjs');

function fixture() {
  return { files: { '.github/scripts/pr-validation-scope.cjs': readFileSync('.github/scripts/pr-validation-scope.cjs', 'utf8') },
    workflows: {
      '.github/workflows/pr-validation.yml': { jobs: { classify: structuredClone(CLASSIFIER_JOB), quality: structuredClone(SCOPED_QUALITY) } },
      '.github/workflows/ci.yml': { on: { workflow_call: { inputs: structuredClone(SCOPE_INPUT), outputs: { 'go-coverage-id': { value: 'retained' } } } }, jobs: {
        native: { if: FULL_IF, steps: [] },
        go: { steps: [{ name: 'Go vulnerability audit' }, ...GO_STEPS.map(name => ({ name, if: FULL_IF }))] },
        checks: { steps: [{ name: 'Require every CI job to succeed', run: SCOPED_AGGREGATE, env: structuredClone(AGGREGATE_ENV) },
          ...CANDIDATE_STEPS.map(name => ({ name, if: FULL_IF }))] },
      } },
    } };
}

test('full validation is preserved for main, release and non-documentation PR work', () => {
  assert.equal(FULL_IF,
    "${{ github.event_name != 'pull_request' || inputs.validation-mode != 'docs-only' }}");
});

test('quality fallback runs after classifier failure while honoring workflow cancellation', () => {
  assert.equal(SCOPED_QUALITY.if, '${{ !cancelled() }}');
  assert.equal(SCOPED_QUALITY.with['validation-mode'],
    "${{ needs.classify.result == 'success' && needs.classify.outputs.validation-mode || 'full' }}");
  const input = fixture();
  input.workflows['.github/workflows/pr-validation.yml'].jobs.quality.if = '${{ always() }}';
  const failures = [];
  normalizeScopedValidation(input.files, input.workflows, failures, 'original aggregate');
  assert.ok(failures.some(error => error.includes('full-fallback quality input')));
});

test('approved PR docs wiring normalizes only expected skips and preserves coverage outputs', () => {
  const input = fixture(); const failures = [];
  const output = normalizeScopedValidation(input.files, input.workflows, failures, 'original aggregate');
  assert.deepEqual(failures, []);
  assert.deepEqual(output['.github/workflows/pr-validation.yml'].jobs, { quality: { uses: './.github/workflows/ci.yml' } });
  const ci = output['.github/workflows/ci.yml'];
  assert.equal(ci.jobs.native.if, undefined);
  assert.ok(ci.jobs.go.steps.every(step => step.if === undefined));
  assert.deepEqual(ci.on.workflow_call.outputs, { 'go-coverage-id': { value: 'retained' } });
  assert.equal(ci.on.workflow_call.inputs, undefined);
  assert.equal(ci.jobs.checks.steps[0].run, 'original aggregate');
  assert.equal(input.workflows['.github/workflows/ci.yml'].jobs.native.if, FULL_IF);
});

test('trusted classifier and full fallback cannot be replaced, overwritten or redirected', () => {
  for (const mutate of [
    input => { input.files['.github/scripts/pr-validation-scope.cjs'] = 'module.exports = { run: async () => {} };'; },
    input => { input.workflows['.github/workflows/pr-validation.yml'].jobs.classify.steps.splice(2, 0, { run: 'overwrite classifier' }); },
    input => { input.workflows['.github/workflows/pr-validation.yml'].jobs.classify.steps[1].with.ref = '${{ github.sha }}'; },
    input => { input.workflows['.github/workflows/pr-validation.yml'].jobs.classify.env = { NODE_OPTIONS: 'malicious' }; },
    input => { input.workflows['.github/workflows/pr-validation.yml'].env = { NODE_OPTIONS: 'malicious' }; },
    input => { input.workflows['.github/workflows/pr-validation.yml'].jobs.quality.with['validation-mode'] = 'docs-only'; },
    input => { input.workflows['.github/workflows/pr-validation.yml'].jobs.quality.with['validation-mode'] = "${{ needs.classify.outputs.validation-mode || 'full' }}"; },
    input => { input.workflows['.github/workflows/ci.yml'].on.workflow_call.inputs['validation-mode'].default = 'docs-only'; },
    input => { input.workflows['.github/workflows/ci.yml'].jobs.go.steps[1].if = "${{ inputs.validation-mode != 'docs-only' }}"; },
    input => { input.workflows['.github/workflows/ci.yml'].jobs.checks.steps[1].if = false; },
  ]) {
    const input = fixture(); mutate(input); const failures = [];
    normalizeScopedValidation(input.files, input.workflows, failures, 'original aggregate');
    assert.ok(failures.length > 0);
  }
});

test('aggregate allows only the authorized PR native skip, never failures or main skips', () => {
  const run = SCOPED_AGGREGATE.replace("python3 - <<'PYTHON'\n", '').replace(/\nPYTHON$/, '');
  const results = Object.fromEntries(['native', 'go', 'frontend', 'scans', 'package', 'docs-ste'].map(job => [job,
    { result: 'success', outputs: job === 'package' ? { 'payload-id': '12', 'qualified-attempt': '1' } : {} }]));
  const execute = (event, mode, input) => execFileSync('python3', ['-c', run], { env: {
    ...process.env, CI_EVENT: event, CI_MODE: mode, CI_NEEDS: JSON.stringify(input),
  }, stdio: 'pipe' });
  execute('push', 'full', results);
  const docs = structuredClone(results); docs.native.result = 'skipped';
  execute('pull_request', 'docs-only', docs);
  assert.throws(() => execute('push', 'docs-only', docs));
  assert.throws(() => execute('workflow_dispatch', 'docs-only', docs));
  assert.throws(() => execute('pull_request', 'full', docs));
  for (const job of Object.keys(results)) {
    for (const status of ['failure', 'cancelled', 'skipped']) {
      if (job === 'native' && status === 'skipped') continue;
      const invalid = structuredClone(docs); invalid[job].result = status;
      assert.throws(() => execute('pull_request', 'docs-only', invalid));
    }
  }
});

test('classifier runtime executes native guarded code and rejects an overwritten workspace module', async () => {
  const outputs = [];
  const AsyncFunction = Object.getPrototypeOf(async function () {}).constructor;
  await new AsyncFunction('require', 'github', 'context', 'core', CLASSIFIER_SCRIPT)(
    require, {}, { eventName: 'push' }, { info() {}, setOutput: (...args) => outputs.push(args) },
  );
  assert.deepEqual(outputs, [['validation-mode', 'full']]);
  const dir = mkdtempSync(join(tmpdir(), 'pr-scope-overwrite-'));
  try {
    const scripts = join(dir, '.github', 'scripts'); mkdirSync(scripts, { recursive: true });
    writeFileSync(join(scripts, 'pr-validation-scope.cjs'),
      "module.exports = { run: async ({core}) => core.setOutput('validation-mode', 'docs-only') };\n");
    const source = `const AsyncFunction = Object.getPrototypeOf(async function () {}).constructor;
new AsyncFunction('require', 'github', 'context', 'core', ${JSON.stringify(CLASSIFIER_SCRIPT)})(
  require, {}, {}, { setOutput: () => console.log('UNAUTHORIZED DOCS') },
).catch(error => { console.error(error.message); process.exitCode = 1; });`;
    assert.throws(() => execFileSync(process.execPath, ['-e', source], { cwd: dir, stdio: 'pipe' }),
      error => error.stderr.toString().includes('classifier bytes changed in the workspace') &&
        !error.stdout.toString().includes('UNAUTHORIZED DOCS'));
  } finally { rmSync(dir, { recursive: true, force: true }); }
});
