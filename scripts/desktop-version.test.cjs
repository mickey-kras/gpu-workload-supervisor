'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const {verifyRelease} = require('./desktop-version.cjs');
const six = release => new Array(6).fill(release);

test('stable and nFPM snapshot package versions match the exact binary build', () => {
  assert.doesNotThrow(() => verifyRelease('1.2.3', six('1.2.3')));
  assert.doesNotThrow(() => verifyRelease('1.2.3~SNAPSHOT-abcd', six('1.2.3-SNAPSHOT-abcd')));
  assert.doesNotThrow(() => verifyRelease('1.2.3~rc.1+build.7', six('1.2.3-rc.1+build.7')));
});

test('mismatched binaries, package versions and development builds are rejected', () => {
  assert.throws(() => verifyRelease('1.2.3', ['1.2.3', '1.2.3', '1.2.3', '1.2.3', '1.2.3', '1.2.4']));
  assert.throws(() => verifyRelease('1.2.3', six('1.2.4')));
  assert.throws(() => verifyRelease('1.2.3-SNAPSHOT-abcd', six('1.2.3-SNAPSHOT-abcd')));
  assert.throws(() => verifyRelease('1.2.3~SNAPSHOT-other', six('1.2.3-SNAPSHOT-abcd')));
  assert.throws(() => verifyRelease('dev', six('dev')));
  assert.throws(() => verifyRelease('1.2.3', six('')));
  assert.throws(() => verifyRelease('1.2.3', new Array(5).fill('1.2.3')));
  assert.throws(() => verifyRelease('1.2.3', ['1.2.3']));
});
