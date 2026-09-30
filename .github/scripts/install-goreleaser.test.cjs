const test = require('node:test');
const assert = require('node:assert/strict');
const { mkdtempSync, mkdirSync, writeFileSync, readFileSync, rmSync, symlinkSync, existsSync } = require('node:fs');
const { tmpdir } = require('node:os');
const { join, resolve } = require('node:path');
const { createHash } = require('node:crypto');
const { execFileSync, spawnSync } = require('node:child_process');

const installer = resolve('.github/scripts/install-goreleaser.sh');
const archive = 'goreleaser_Linux_x86_64.tar.gz';

function fixture(t, failure = '') {
  const dir = mkdtempSync(join(tmpdir(), 'goreleaser-test-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const bin = join(dir, 'bin');
  const assets = join(dir, 'assets');
  mkdirSync(bin);
  mkdirSync(assets);
  for (const command of ['mktemp', 'rm', 'awk', 'sha256sum', 'gzip', 'install']) {
    const commandPath = execFileSync('/bin/bash', ['-c', `command -v ${command}`], { encoding: 'utf8' }).trim();
    symlinkSync(commandPath, join(bin, command));
  }
  const tarPath = execFileSync('/bin/bash', ['-c', 'command -v tar'], { encoding: 'utf8' }).trim();
  writeFileSync(join(bin, 'tar'), `#!/bin/bash
printf extracted > "$EXTRACTED"
exec ${tarPath} "$@"
`, { mode: 0o755 });
  writeFileSync(join(assets, 'goreleaser'), '#!/bin/bash\nprintf "verified fixture\\n"\n');
  execFileSync('tar', ['-czf', join(assets, archive), '-C', assets, 'goreleaser']);
  const digest = createHash('sha256').update(readFileSync(join(assets, archive))).digest('hex');
  writeFileSync(join(assets, 'checksums.txt'), `${digest}  ${archive}\n`);
  writeFileSync(join(assets, 'checksums.txt.sigstore.json'), 'fixture-signature');
  if (failure === 'tampered-archive') writeFileSync(join(assets, archive), 'modified after signing');
  if (failure === 'missing-entry') writeFileSync(join(assets, 'checksums.txt'), `${digest}  another-archive\n`);
  if (failure === 'duplicate-entry') writeFileSync(join(assets, 'checksums.txt'), `${digest}  ${archive}\n`.repeat(2));
  writeFileSync(join(bin, 'curl'), `#!/bin/bash
set -eu
while [[ "$1" != --output ]]; do shift; done
file="$2"
url="$3"
[[ "$url" == "https://github.com/goreleaser/goreleaser/releases/download/v2.18.2/$file" ]]
[[ "$FAILURE" != "missing-$file" ]] || exit 22
/bin/cp "$ASSETS/$file" "$file"
`, { mode: 0o755 });
  if (failure !== 'missing-cosign') {
    writeFileSync(join(bin, 'cosign'), `#!/bin/bash
set -eu
[[ "$*" == 'verify-blob --certificate-identity https://github.com/goreleaser/goreleaser/.github/workflows/release.yml@refs/tags/v2.18.2 --certificate-oidc-issuer https://token.actions.githubusercontent.com --bundle checksums.txt.sigstore.json checksums.txt' ]]
[[ "$FAILURE" != invalid-signature ]] || exit 1
printf 'verified' > "$VERIFIED"
`, { mode: 0o755 });
  }
  const env = { ...process.env, PATH: bin, ASSETS: assets, FAILURE: failure,
    RUNNER_TEMP: dir, RUNNER_OS: 'Linux', RUNNER_ARCH: 'X64',
    GITHUB_PATH: join(dir, 'github-path'), EXTRACTED: join(dir, 'extracted'), VERIFIED: join(dir, 'verified') };
  return { dir, env, run: () => spawnSync('/bin/bash', [installer, 'v2.18.2'], { env, encoding: 'utf8' }) };
}

test('installs only the archive matched by authenticated checksums', t => {
  const f = fixture(t);
  const result = f.run();
  assert.equal(result.status, 0, result.stderr);
  assert.ok(existsSync(f.env.VERIFIED));
  const installDir = readFileSync(f.env.GITHUB_PATH, 'utf8').trim();
  assert.equal(execFileSync(join(installDir, 'goreleaser'), { encoding: 'utf8' }), 'verified fixture\n');
});

for (const failure of ['missing-cosign', 'missing-checksums.txt', 'missing-checksums.txt.sigstore.json',
  `missing-${archive}`, 'invalid-signature', 'tampered-archive', 'missing-entry', 'duplicate-entry']) {
  test(`fails closed before installation: ${failure}`, t => {
    const f = fixture(t, failure);
    const result = f.run();
    assert.notEqual(result.status, 0, result.stdout);
    assert.equal(existsSync(f.env.GITHUB_PATH), false);
    assert.equal(existsSync(f.env.EXTRACTED), false);
  });
}
