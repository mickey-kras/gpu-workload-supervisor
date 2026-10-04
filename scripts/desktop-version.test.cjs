'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const {verifyRelease} = require('./desktop-version.cjs');
const four = release => new Array(4).fill(release);

test('stable and nFPM snapshot package versions match the exact binary build', () => {
  assert.doesNotThrow(() => verifyRelease('1.2.3', four('1.2.3')));
  assert.doesNotThrow(() => verifyRelease('1.2.3~SNAPSHOT-abcd', four('1.2.3-SNAPSHOT-abcd')));
  assert.doesNotThrow(() => verifyRelease('1.2.3~rc.1+build.7', four('1.2.3-rc.1+build.7')));
});

test('mismatched binaries, package versions and development builds are rejected', () => {
  assert.throws(() => verifyRelease('1.2.3', ['1.2.3', '1.2.3', '1.2.4', '1.2.3']));
  assert.throws(() => verifyRelease('1.2.3', four('1.2.4')));
  assert.throws(() => verifyRelease('1.2.3-SNAPSHOT-abcd', four('1.2.3-SNAPSHOT-abcd')));
  assert.throws(() => verifyRelease('1.2.3~SNAPSHOT-other', four('1.2.3-SNAPSHOT-abcd')));
  assert.throws(() => verifyRelease('dev', four('dev')));
  assert.throws(() => verifyRelease('1.2.3', four('')));
  assert.throws(() => verifyRelease('1.2.3', ['1.2.3']));
});
