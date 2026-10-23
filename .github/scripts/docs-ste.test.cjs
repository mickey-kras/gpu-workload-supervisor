const test = require('node:test');
const assert = require('node:assert/strict');
const { readFileSync, mkdtempSync, mkdirSync, writeFileSync, readdirSync, rmSync, symlinkSync } = require('node:fs');
const { tmpdir } = require('node:os');
const { join, dirname } = require('node:path');
const { execFileSync, spawnSync } = require('node:child_process');
const YAML = require('yaml');

const workflow = YAML.parse(readFileSync('.github/workflows/docs-ste.yml', 'utf8'));
const included = ['README.md', 'root.MARKDOWN', '.hidden/guide.Md', 'nested/with spaces/guide.markdown',
  'case.md', 'case.MD', 'docs/releases/release.md', 'untracked.md', 'CHANGELOG.md',
  'build/instructions.md', 'generated/guide.md'];
const excluded = ['LICENSE.md', 'clients/setup/icons/LICENSES.md',
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

  test(`${id} fails for a Markdown symlink`, () => {
    withRepository((repo, temp) => {
      writeFileSync(join(repo, 'target'), 'The target is outside Markdown discovery.\n');
      symlinkSync('target', join(repo, 'guide.md'));
      const result = spawnSync('bash', ['-c', script], { cwd: repo, env: { ...process.env, RUNNER_TEMP: temp }, encoding: 'utf8' });
      assert.notEqual(result.status, 0);
    });
  });
}
