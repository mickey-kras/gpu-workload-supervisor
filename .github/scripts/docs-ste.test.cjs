const test = require('node:test');
const assert = require('node:assert/strict');
const { readFileSync, mkdtempSync, mkdirSync, writeFileSync, readdirSync, rmSync, symlinkSync } = require('node:fs');
const { tmpdir } = require('node:os');
const { join, dirname } = require('node:path');
const { execFileSync, spawnSync } = require('node:child_process');
const YAML = require('yaml');
const { runInNewContext } = require('node:vm');

const workflow = YAML.parse(readFileSync('.github/workflows/docs-ste.yml', 'utf8'));
const included = ['README.md', 'root.MARKDOWN', '.hidden/guide.Md', 'nested/with spaces/guide.markdown',
  'case.md', 'case.MD', 'docs/releases/release.md', 'untracked.md', 'CHANGELOG.md',
  'build/instructions.md', 'generated/guide.md', 'LICENSE.md',
  'docs/licenses/operator.md', 'docs/LICENSES.md', 'docs/THIRD_PARTY_NOTICES.md'];
const excluded = ['clients/setup/icons/LICENSES.md',
  'clients/setup/icons/vllm-usage-notice.md', 'node_modules/package/README.md', '.vale/styles/STE/README.md'];

function withRepository(run) {
  const root = mkdtempSync(join(tmpdir(), 'docs-ste-'));
  try {
    const repo = join(root, 'repository'); mkdirSync(repo);
    const temp = join(root, 'runner'); mkdirSync(temp);
    execFileSync('git', ['init', '-q', repo]);
    run(repo, temp);
  } finally { rmSync(root, { recursive: true, force: true }); }
}

for (const id of ['ste-cli', 'vale']) {
  const script = workflow.jobs[id].steps.find(step => step.name === 'Prepare documentation').run;
  test(`${id} discovers new root, hidden, mixed-case and space-containing Markdown without collisions`, () => {
    withRepository((repo, temp) => {
      for (const path of [...included, ...excluded]) {
        mkdirSync(dirname(join(repo, path)), { recursive: true });
        writeFileSync(join(repo, path), `# ${path}\n\nThe file has unique text.\n`);
      }
      execFileSync('git', ['add', '--', ...included.filter(path => path !== 'untracked.md')], { cwd: repo });
      const result = spawnSync('bash', ['-c', script], { cwd: repo, env: { ...process.env, RUNNER_TEMP: temp }, encoding: 'utf8' });
      assert.equal(result.status, 0, result.stderr);
      const stage = join(temp, 'docs-ste');
      const paths = readdirSync(stage, { recursive: true }).filter(path => path.endsWith('.md')).sort();
      assert.deepEqual(paths, included.map(path => `${path}.md`).sort());
      for (const path of included) assert.equal(readFileSync(join(stage, `${path}.md`), 'utf8'), readFileSync(join(repo, path), 'utf8'));
    });
  });


  test(`${id} rejects blanket, rule, match and style directives before linting`, () => {
    for (const directive of [
      '<!-- ste-disable -->', '<!--ste-disable STE-3.6-->', '<!-- ste-disable-next-line -->',
      '<!-- ste-disable-line STE-3.6 -->', '<!-- ste-enable -->', '<!-- ste-disable',
      '<!--\tste-disable-next-line\tSTE-3.6 -->', '<!--\u00a0ste-disable\u2003STE-3.6 -->', '<!-- STE-DISABLE -->',
      '<!-- vale off -->', '<!-- vale on -->', '<!-- VALE OFF -->', '<!--\nvale off\n-->',
      '<!-- vale STE = NO -->', '<!-- vale STE.PassiveVoice = off -->',
      '<!-- vale STE.Dictionary["utilize"] = NO -->', '<!-- vale style = Vale -->',
      '<!-- vale styles = Vale, Other -->', '<!-- vale STE.PassiveVoice = YES -->',
    ]) {
      withRepository((repo, temp) => {
        writeFileSync(join(repo, 'guide.md'), `${directive}\nWe can't utilize this system; the file was deleted by the operator.\n`);
        const result = spawnSync('bash', ['-c', script], { cwd: repo, env: { ...process.env, RUNNER_TEMP: temp }, encoding: 'utf8' });
        assert.notEqual(result.status, 0, directive);
        assert.match(result.stderr, /linter control comments are not permitted/);
      });
    }
  });

  test(`${id} permits ordinary HTML comments and leaves bold prose visible`, () => {
    withRepository((repo, temp) => {
      const content = '<!-- This is an ordinary comment. -->\n**You should restart the service.**\n';
      writeFileSync(join(repo, 'guide.md'), content);
      const result = spawnSync('bash', ['-c', script], { cwd: repo, env: { ...process.env, RUNNER_TEMP: temp }, encoding: 'utf8' });
      assert.equal(result.status, 0, result.stderr);
      assert.equal(readFileSync(join(temp, 'docs-ste', 'guide.md.md'), 'utf8'), content);
    });
  });

  test(`${id} fails for a Markdown symlink`, () => {
    withRepository((repo, temp) => {
      writeFileSync(join(repo, 'target'), 'The target is outside Markdown discovery.\n');
      symlinkSync('target', join(repo, 'guide.md'));
      const result = spawnSync('bash', ['-c', script], { cwd: repo, env: { ...process.env, RUNNER_TEMP: temp }, encoding: 'utf8' });
      assert.notEqual(result.status, 0);
    });
  });
}

test('PR updates replace the same STE run while main validation keeps each run', () => {
  const render = (value, github) => value.replace(/\$\{\{(.*?)\}\}/g,
    (_, expression) => String(runInNewContext(expression, { github })));
  const context = (eventName, runID) => ({ workflow: eventName === 'pull_request' ? 'pr' : 'main',
    event_name: eventName, run_id: runID, event: { pull_request: eventName === 'pull_request' ? { number: 610 } : {} } });
  const firstPR = context('pull_request', 100); const nextPR = context('pull_request', 101);
  assert.equal(render(workflow.concurrency.group, firstPR), render(workflow.concurrency.group, nextPR));
  assert.equal(render(workflow.concurrency['cancel-in-progress'], nextPR), 'true');
  for (const eventName of ['push', 'workflow_dispatch']) {
    const first = context(eventName, 100); const next = context(eventName, 101);
    assert.notEqual(render(workflow.concurrency.group, first), render(workflow.concurrency.group, next));
    assert.equal(render(workflow.concurrency['cancel-in-progress'], next), 'false');
  }
});
