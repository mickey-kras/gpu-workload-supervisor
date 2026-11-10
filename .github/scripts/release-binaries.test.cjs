const test = require('node:test');
const assert = require('node:assert/strict');
const { readFileSync, readdirSync } = require('node:fs');
const YAML = require('yaml');

// packaging/release-binaries.json is the one release-binary inventory. Every
// other list that names release binaries must agree with it, so a binary added
// in one place fails CI until it is built, packaged and version-checked.
const inventory = JSON.parse(readFileSync('packaging/release-binaries.json', 'utf8')).binaries;
const names = inventory.map(binary => binary.name);
const RELEASE_LDFLAG = '-s -w -X github.com/mickey-kras/gpu-workload-supervisor/internal/deployment.Release={{ .Version }}';

test('release inventory is unique and matches every cmd/ entry point', () => {
  assert.ok(inventory.every(binary => Object.keys(binary).sort().join() === 'activated,name' &&
    /^gpu-[a-z-]+$/.test(binary.name) && typeof binary.activated === 'boolean'));
  assert.equal(new Set(names).size, names.length);
  const commands = readdirSync('cmd', { withFileTypes: true }).filter(entry => entry.isDirectory())
    .map(entry => entry.name);
  assert.deepEqual([...commands].sort(), [...names].sort());
});

test('GoReleaser builds, archives and the desktop package carry exactly the inventory', () => {
  const config = YAML.parse(readFileSync('.goreleaser.yaml', 'utf8'));
  assert.deepEqual(config.builds.map(build => build.id), names);
  for (const build of config.builds) {
    assert.equal(build.binary, build.id);
    assert.equal(build.main, `./cmd/${build.id}`);
    assert.deepEqual(build.ldflags, [RELEASE_LDFLAG]);
    assert.deepEqual(build.env, ['CGO_ENABLED=0']);
    assert.deepEqual(build.goos, ['linux']);
    assert.deepEqual(build.goarch, ['amd64', 'arm64']);
  }
  // Archives and packages default to every build; an ids filter would silently drop binaries.
  assert.deepEqual(config.nfpms.map(nfpm => nfpm.id), ['desktop']);
  assert.ok([...config.archives, ...config.nfpms].every(item =>
    !Object.hasOwn(item, 'ids') && !Object.hasOwn(item, 'builds')));
});

test('package checks and notices read the inventory instead of a hardcoded list', () => {
  const check = readFileSync('scripts/check-desktop-package.sh', 'utf8');
  assert.match(check, /node "\$version_check" --list/);
  assert.match(check, /for binary in "\$\{binaries\[@\]\}"; do/);
  assert.doesNotMatch(check, /for binary in gpu-/);
  const notices = readFileSync('scripts/third-party-notices.sh', 'utf8');
  assert.match(notices, /node scripts\/desktop-version\.cjs --list/);
  assert.doesNotMatch(notices, /\.\/cmd\/gpu-/);
  const { releaseBinaries } = require('../../scripts/desktop-version.cjs');
  assert.deepEqual(releaseBinaries(), names);
});

test('setup retains exactly the activated subset of the inventory', () => {
  // Setup keeps rollback copies only of the binaries its units execute; the
  // training tools are invoked on demand and are intentionally not retained.
  const source = readFileSync('internal/setup/binaries.go', 'utf8');
  const declaration = source.match(/^var binaries = \[\]string\{([^}]*)\}$/m);
  assert.ok(declaration, 'internal/setup/binaries.go must declare binaries as one string slice literal');
  const retained = [...declaration[1].matchAll(/"([^"]+)"/g)].map(match => match[1]);
  assert.deepEqual(retained, inventory.filter(binary => binary.activated).map(binary => binary.name));
});
