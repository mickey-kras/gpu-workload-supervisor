'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const {verifyRelease} = require('./desktop-version.cjs');
const five = release => new Array(5).fill(release);

test('stable and nFPM snapshot package versions match the exact binary build', () => {
  assert.doesNotThrow(() => verifyRelease('1.2.3', five('1.2.3')));
  assert.doesNotThrow(() => verifyRelease('1.2.3~SNAPSHOT-abcd', five('1.2.3-SNAPSHOT-abcd')));
  assert.doesNotThrow(() => verifyRelease('1.2.3~rc.1+build.7', five('1.2.3-rc.1+build.7')));
});

test('mismatched binaries, package versions and development builds are rejected', () => {
  assert.throws(() => verifyRelease('1.2.3', ['1.2.3', '1.2.3', '1.2.3', '1.2.3', '1.2.4']));
  assert.throws(() => verifyRelease('1.2.3', five('1.2.4')));
  assert.throws(() => verifyRelease('1.2.3-SNAPSHOT-abcd', five('1.2.3-SNAPSHOT-abcd')));
  assert.throws(() => verifyRelease('1.2.3~SNAPSHOT-other', five('1.2.3-SNAPSHOT-abcd')));
  assert.throws(() => verifyRelease('dev', five('dev')));
  assert.throws(() => verifyRelease('1.2.3', five('')));
  assert.throws(() => verifyRelease('1.2.3', new Array(4).fill('1.2.3')));
  assert.throws(() => verifyRelease('1.2.3', ['1.2.3']));
});
