const test = require('node:test');
const assert = require('node:assert/strict');
const { inspect } = require('./policy-guard.cjs');

test('removed policy files and unpinned actions fail closed', () => {
  const errors = inspect({
    '.github/workflows/example.yml': 'steps:\n  - uses: actions/checkout@v7\n',
  });
  assert.ok(errors.some(error => error.includes('Missing required file')));
  assert.ok(errors.some(error => error.includes('unpin')));
});
