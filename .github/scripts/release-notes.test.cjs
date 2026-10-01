const test = require('node:test');
const assert = require('node:assert/strict');
const { mkdtempSync, mkdirSync, writeFileSync, readFileSync, rmSync } = require('node:fs');
const { tmpdir } = require('node:os');
const { join } = require('node:path');
const { spawnSync } = require('node:child_process');
const YAML = require('yaml');

for (const authored of [false, true]) {
  test(`release publication ${authored ? 'includes authored notes' : 'retains generated notes without an authored file'}`, () => {
    const directory = mkdtempSync(join(tmpdir(), 'release-notes-'));
    try {
      mkdirSync(join(directory, 'release-assets'));
      writeFileSync(join(directory, 'release-assets', 'binary.tar.gz'), 'fixture');
      if (authored) {
        mkdirSync(join(directory, 'docs/releases'), { recursive: true });
        writeFileSync(join(directory, 'docs/releases/v0.1.5.md'), 'Migration and rollback notes');
      }
      const workflow = YAML.parse(readFileSync('.github/workflows/release.yml', 'utf8'));
      const step = workflow.jobs.publish.steps.find(item => item.name === 'Publish immutable GitHub release');
      const result = spawnSync('bash', ['-c', 'gh() { printf "%s\\0" "$@"; };\n' + step.run], {
        cwd: directory,
        env: { ...process.env, VERSION: '0.1.5', GITHUB_REPOSITORY: 'owner/repo', RELEASE_SHA: 'verified-sha' },
        encoding: 'utf8',
      });
      assert.equal(result.status, 0, result.stderr);
      const args = result.stdout.split('\0').filter(Boolean);
      assert.deepEqual(args, [
        'release', 'create', 'v0.1.5', 'release-assets/binary.tar.gz',
        '--repo', 'owner/repo', '--target', 'verified-sha', '--title', 'v0.1.5', '--generate-notes',
        ...(authored ? ['--notes-file', 'docs/releases/v0.1.5.md'] : []),
      ]);
    } finally {
      rmSync(directory, { recursive: true, force: true });
    }
  });
}
