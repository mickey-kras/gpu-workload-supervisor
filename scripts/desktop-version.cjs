'use strict';
const {readFileSync} = require('node:fs');
const {join} = require('node:path');

// packaging/release-binaries.json is the single release-binary inventory.
// GoReleaser, the package check and setup's activated subset are verified
// against it by .github/scripts/release-binaries.test.cjs.
function releaseBinaries(path = join(__dirname, '..', 'packaging', 'release-binaries.json')) {
  const names = JSON.parse(readFileSync(path, 'utf8')).binaries.map(binary => binary.name);
  if (!names.length || new Set(names).size !== names.length || names.some(name => !/^gpu-[a-z-]+$/.test(name)))
    throw new Error('Release binary inventory must list unique gpu-* binaries');
  return names;
}

// nFPM emits SemVer prereleases after '~' in Debian control metadata. This
// package does not configure a Debian epoch or revision.
function verifyRelease(packageVersion, releases, expected = releaseBinaries().length) {
  if (releases.length !== expected || releases.some(value => value !== releases[0]))
    throw new Error(`The ${expected} packaged binaries must have one identical release`);
  const release = releases[0];
  const semver = /^(\d+\.\d+\.\d+)(?:-([0-9A-Za-z.-]+))?(\+[0-9A-Za-z.-]+)?$/.exec(release);
  if (!semver)
    throw new Error('Packaged binaries require a non-development release version');
  const debianVersion = semver[1] + (semver[2] ? `~${semver[2]}` : '') + (semver[3] ?? '');
  if (debianVersion !== packageVersion)
    throw new Error('Package version differs from its embedded binary release');
}

module.exports = {releaseBinaries, verifyRelease};
if (require.main === module) {
  try {
    if (process.argv[2] === '--list') {
      process.stdout.write(`${releaseBinaries().join('\n')}\n`);
    } else {
      verifyRelease(process.argv[2], process.argv.slice(3));
    }
  } catch (error) {
    process.stderr.write(`${error.message}\n`);
    process.exitCode = 1;
  }
}
