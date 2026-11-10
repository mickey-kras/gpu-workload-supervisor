'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const {execFileSync} = require('node:child_process');
const {mkdtempSync, rmSync, writeFileSync} = require('node:fs');
const {tmpdir} = require('node:os');
const {join} = require('node:path');
const {releaseBinaries, verifyRelease} = require('./desktop-version.cjs');
const count = releaseBinaries().length;
const all = release => new Array(count).fill(release);
const cli = join(__dirname, 'desktop-version.cjs');

test('stable and nFPM snapshot package versions match the exact binary build', () => {
  assert.doesNotThrow(() => verifyRelease('1.2.3', all('1.2.3')));
  assert.doesNotThrow(() => verifyRelease('1.2.3~SNAPSHOT-abcd', all('1.2.3-SNAPSHOT-abcd')));
  assert.doesNotThrow(() => verifyRelease('1.2.3~rc.1+build.7', all('1.2.3-rc.1+build.7')));
});

test('mismatched binaries, package versions and development builds are rejected', () => {
  assert.throws(() => verifyRelease('1.2.3', [...all('1.2.3').slice(1), '1.2.4']));
  assert.throws(() => verifyRelease('1.2.3', all('1.2.4')));
  assert.throws(() => verifyRelease('1.2.3-SNAPSHOT-abcd', all('1.2.3-SNAPSHOT-abcd')));
  assert.throws(() => verifyRelease('1.2.3~SNAPSHOT-other', all('1.2.3-SNAPSHOT-abcd')));
  assert.throws(() => verifyRelease('dev', all('dev')));
  assert.throws(() => verifyRelease('1.2.3', all('')));
  assert.throws(() => verifyRelease('1.2.3', new Array(count - 1).fill('1.2.3')), /packaged binaries/);
  assert.throws(() => verifyRelease('1.2.3', ['1.2.3']));
});

test('the release inventory lists every packaged binary, including gpu-training-import', () => {
  assert.ok(releaseBinaries().includes('gpu-training-import'));
  const dir = mkdtempSync(join(tmpdir(), 'release-binaries-'));
  try {
    for (const binaries of [[], [{name: 'gpu-mode'}, {name: 'gpu-mode'}], [{name: '../evil'}]]) {
      const path = join(dir, 'inventory.json');
      writeFileSync(path, JSON.stringify({binaries}));
      assert.throws(() => releaseBinaries(path), /unique gpu-\* binaries/);
    }
  } finally {
    rmSync(dir, {recursive: true, force: true});
  }
});

test('command line lists the inventory and verifies embedded releases', () => {
  assert.deepEqual(execFileSync(process.execPath, [cli, '--list'], {encoding: 'utf8'}).trim().split('\n'),
    releaseBinaries());
  assert.doesNotThrow(() => execFileSync(process.execPath, [cli, '1.2.3', ...all('1.2.3')], {stdio: 'pipe'}));
  assert.throws(() => execFileSync(process.execPath, [cli, '1.2.3', '1.2.3'], {stdio: 'pipe'}),
    error => error.status === 1 && error.stderr.toString().includes('packaged binaries'));
});
