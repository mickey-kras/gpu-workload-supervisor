const test = require('node:test');
const assert = require('node:assert/strict');
const { verifyPublished } = require('./release-publication.cjs');
const { releaseVersion } = require('./release-follow-up.cjs');

test('native publication version validation preserves the plain-SemVer contract and numeric bounds', async () => {
  for (const version of ['0.1.0', '1.2.3', '9007199254740991.0.0']) {
    assert.equal(releaseVersion(JSON.stringify({ version })), version);
    await assert.rejects(verifyPublished({ context: { sha: 'invalid' }, version }), /Invalid release commit/);
  }
  for (const version of ['v0.1.0', '01.1.0', '0.1', '0.1.0-beta', '0.1.0+build',
    '9007199254740992.0.0', '0.' + '1'.repeat(256) + '.0', '', 1, null]) {
    assert.throws(() => releaseVersion(JSON.stringify({ version })), /plain SemVer/);
    await assert.rejects(verifyPublished({ context: { sha: 'invalid' }, version }), /plain SemVer/);
  }
});
