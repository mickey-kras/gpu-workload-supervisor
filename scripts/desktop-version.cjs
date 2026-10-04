'use strict';

// nFPM emits SemVer prereleases after '~' in Debian control metadata. This
// package does not configure a Debian epoch or revision.
function verifyRelease(packageVersion, releases) {
  if (releases.length !== 4 || releases.some(value => value !== releases[0]))
    throw new Error('The four packaged binaries must have one identical release');
  const release = releases[0];
  const semver = /^(\d+\.\d+\.\d+)(?:-([0-9A-Za-z.-]+))?(\+[0-9A-Za-z.-]+)?$/.exec(release);
  if (!semver)
    throw new Error('Packaged binaries require a non-development release version');
  const debianVersion = semver[1] + (semver[2] ? `~${semver[2]}` : '') + (semver[3] ?? '');
  if (debianVersion !== packageVersion)
    throw new Error('Package version differs from its embedded binary release');
}

module.exports = {verifyRelease};
if (require.main === module) {
  try {
    verifyRelease(process.argv[2], process.argv.slice(3));
  } catch (error) {
    process.stderr.write(`${error.message}\n`);
    process.exitCode = 1;
  }
}
