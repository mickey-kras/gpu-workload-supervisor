const test = require('node:test');
const assert = require('node:assert/strict');
const { mkdtempSync, mkdirSync, readFileSync, writeFileSync, rmSync } = require('node:fs');
const { tmpdir } = require('node:os');
const { join } = require('node:path');
const { execFileSync } = require('node:child_process');
const YAML = require('yaml');

test('release manifest covers archives, SBOM and both desktop packages', () => {
  const workflow = YAML.parse(readFileSync('.github/workflows/release.yml', 'utf8'));
  const checksum = workflow.jobs.publish.steps.find(step => step.name === 'Checksum all release assets');
  const dir = mkdtempSync(join(tmpdir(), 'release-checksums-'));
  const assets = ['gpu-workload-supervisor-v0.1.0.tar.gz',
    'gpu-workload-supervisor_0.1.0_linux_amd64.tar.gz',
    'gpu-workload-supervisor_0.1.0_linux_arm64.tar.gz',
    'gpu-workload-supervisor_0.1.0_linux_amd64.deb',
    'gpu-workload-supervisor_0.1.0_linux_arm64.deb', 'sbom.cdx.json'];
  try {
    mkdirSync(join(dir, 'release-assets'));
    for (const name of assets) writeFileSync(join(dir, 'release-assets', name), name);
    execFileSync('bash', ['-e', '-c', checksum.run], { cwd: dir, stdio: 'pipe' });
    const manifest = readFileSync(join(dir, 'release-assets', 'SHA256SUMS'), 'utf8');
    assert.deepEqual(manifest.trim().split('\n').map(line => line.slice(66)).sort(), assets.sort());
    for (const name of assets.filter(name => name.endsWith('.deb'))) {
      writeFileSync(join(dir, 'release-assets', name), 'tampered');
      assert.throws(() => execFileSync('sha256sum', ['--check', 'SHA256SUMS'],
        { cwd: join(dir, 'release-assets'), stdio: 'pipe' }));
      writeFileSync(join(dir, 'release-assets', name), name);
    }
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
