const test = require('node:test');
const assert = require('node:assert/strict');
const { FORBIDDEN_INPUTS, LOCKFILES, inspectLockfiles } = require('./policy-lockfiles.cjs');

function fixture() {
  const manifest = { name: 'ci', version: '1.0.0', dependencies: { example: '1.0.0' } };
  const lock = { lockfileVersion: 3, packages: {
    '': manifest,
    'node_modules/example': {
      version: '1.0.0', resolved: 'https://registry.npmjs.org/example/-/example-1.0.0.tgz',
      integrity: 'sha512-' + Buffer.alloc(64, 1).toString('base64'),
    },
  } };
  return Object.fromEntries(LOCKFILES.flatMap(path => [
    [path, JSON.stringify(lock)], [path.replace('package-lock.json', 'package.json'), JSON.stringify(manifest)],
  ]));
}

function check(mutate, admitted = new Set()) {
  const base = fixture();
  const files = { ...base };
  const lock = JSON.parse(files[LOCKFILES[0]]);
  mutate(lock, files);
  files[LOCKFILES[0]] = JSON.stringify(lock);
  const failures = [];
  inspectLockfiles(files, base, failures, admitted);
  return failures;
}

test('registry package upgrades need no policy hash edit', () => {
  assert.deepEqual(check(lock => {
    lock.packages['node_modules/example'].version = '1.0.1';
    lock.packages['node_modules/example'].resolved = 'https://registry.npmjs.org/example/-/example-1.0.1.tgz';
  }), []);
});

test('npm configuration and shrinkwrap cannot redirect an unchanged validated lockfile', () => {
  for (const path of FORBIDDEN_INPUTS) {
    const files = { ...fixture(), [path]: 'registry=https://evil.test\n' };
    const failures = [];
    inspectLockfiles(files, fixture(), failures, new Set(Object.keys(files)));
    assert.ok(failures.some(message => message.includes('input redirection')), path);
  }
});

test('rejects registry lookalikes, local sources, noncanonical integrity and executable metadata', () => {
  for (const patch of [
    { resolved: 'https://registry.npmjs.org.evil.test/example/-/example-1.0.0.tgz' },
    { resolved: 'https://evil.test@registry.npmjs.org/example/-/example-1.0.0.tgz' },
    { resolved: 'http://registry.npmjs.org/example/-/example-1.0.0.tgz' },
    { resolved: 'https://registry.npmjs.org/example/-/example-1.0.0.tgz?redirect=evil' },
    { resolved: 'file:../example' }, { resolved: 'git+https://example.test/repo' },
    { integrity: 'sha1-' + Buffer.alloc(20).toString('base64') }, { integrity: undefined },
    { integrity: 'sha512-' + 'A'.repeat(85) + 'B==' },
    { link: true }, { inBundle: true }, { hasInstallScript: true }, { hasInstallScript: 'false' },
  ]) assert.ok(check(lock => Object.assign(lock.packages['node_modules/example'], patch)).length, JSON.stringify(patch));
});

test('existing install-script flags are compared to the immutable base', () => {
  const files = fixture();
  const lock = JSON.parse(files[LOCKFILES[0]]);
  lock.packages['node_modules/example'].hasInstallScript = true;
  files[LOCKFILES[0]] = JSON.stringify(lock);
  const failures = [];
  inspectLockfiles(files, files, failures);
  assert.deepEqual(failures, []);
});

test('manifest and lock roots remain bound, even for approved changes', () => {
  const path = LOCKFILES[0].replace('package-lock.json', 'package.json');
  const mutate = (lock, files) => {
    const manifest = JSON.parse(files[path]);
    manifest.dependencies.example = '2.0.0';
    lock.packages[''].dependencies.example = '2.0.0';
    files[path] = JSON.stringify(manifest);
  };
  assert.ok(check(mutate).length);
  assert.deepEqual(check(mutate, new Set([path])), []);
  assert.ok(check(lock => { lock.packages[''].dependencies.example = 'evil'; }, new Set([path])).length);
});

test('malformed packages, path escapes, duplicate keys and legacy locks fail closed', () => {
  for (const mutate of [
    lock => { lock.lockfileVersion = 2; },
    lock => { delete lock.packages['']; },
    lock => { lock.packages['node_modules/example'] = null; },
    lock => { lock.packages['../escape'] = lock.packages['node_modules/example']; },
  ]) assert.ok(check(mutate).length);
  const files = fixture();
  files[LOCKFILES[0]] = '{"lockfileVersion":3,"lockfileVersion":2,"packages":{}}';
  const failures = [];
  inspectLockfiles(files, fixture(), failures);
  assert.match(failures[0], /duplicate JSON keys/);
});
