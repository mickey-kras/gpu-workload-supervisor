const test = require('node:test');
const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const { inspect } = require('./policy-guard.cjs');

const path = '.github/workflows/policy-guard.yml';
const admission = `${path}: policy execution sources require independent exact-head owner admission`;

test('policy guard admits only harden-runner pin and comment changes without review', () => {
  const before = readFileSync(path, 'utf8');
  const action = before.split('\n').find(line => line.trim().startsWith('- uses: step-security/harden-runner@'));
  const prefix = action.split('@')[0] + '@';
  for (const changed of [
    before.replace(action, prefix + 'f'.repeat(40)),
    before.replace(action, prefix + 'f'.repeat(40) + ' # v2.22.2'),
    before.replace(action, action.split('#')[0].trimEnd() + ' # v2.22.2'),
  ]) {
    const errors = inspect({ [path]: changed }, { [path]: before });
    assert.ok(!errors.includes(admission));
  }
  for (const [mutation, changed] of [
    ['trigger', before.replace('pull_request_target', 'workflow_run')],
    ['permission', before.replace('contents: read', 'contents: write')],
    ['execution', before.replace("await require('./.github/scripts/policy-guard.cjs').run({ github, context });", 'return;')],
    ['other action', before.replace(/actions\/checkout@[a-f0-9]{40}/, 'actions/checkout@' + 'f'.repeat(40))],
    ['malformed pin', before.replace(action, prefix + 'v2.22.2')],
  ]) {
    const errors = inspect({ [path]: changed }, { [path]: before });
    assert.ok(errors.includes(admission), mutation);
  }
});
