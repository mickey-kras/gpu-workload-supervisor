const test = require('node:test');
const assert = require('node:assert/strict');
const { ESLint } = require('eslint');
const config = require('./eslint.config.cjs');

const eslint = new ESLint({ overrideConfigFile: true, overrideConfig: config });
const filePath = 'clients/setup/maintainability-fixture.mjs';

for (const [rule, source] of [
  ['no-nested-conditional', 'const status = busy ? "busy" : ready ? "ready" : "idle";'],
  ['no-nested-template-literals', 'const detail = `status: ${`ready ${name}`}`;'],
  ['no-nested-functions', 'const nested = () => () => () => () => () => 1;'],
  ['no-unenclosed-multiline-block', 'if (ready) update(); save();'],
]) {
  test(`PR lint rejects ${rule}`, async () => {
    const [result] = await eslint.lintText(source, { filePath });
    assert.ok(result.messages.some(message =>
      message.ruleId === `sonarjs/${rule}` && message.severity === 2));
  });
}

test('independent conditionals, named template details and braced if pass', async () => {
  const [result] = await eslint.lintText(`
    let status = 'idle';
    if (ready) { status = 'ready'; }
    if (busy) { status = 'busy'; }
    const label = 'ready ' + name;
    const detail = \`status: \${label}\`;
    if (ready) { update(); save(); }
  `, { filePath });
  assert.deepEqual(result.messages, []);
});

test('inline disables cannot hide production regressions', async () => {
  const [result] = await eslint.lintText(
    '/* eslint-disable */ const status = busy ? "busy" : ready ? "ready" : "idle";',
    { filePath });
  assert.ok(result.errorCount > 0);
});
