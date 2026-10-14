const test = require('node:test');
const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const { resolve } = require('node:path');
const { pins, propose } = require('./build-tool-updates.cjs');

const root = resolve(__dirname, '../..');
const files = Object.fromEntries(pins.map(pin => [pin.path, readFileSync(resolve(root, pin.path), 'utf8')]));
const stable = tag_name => ({ tag_name, draft: false, prerelease: false });
const current = repository => {
  const pin = pins.find(item => item.repo === repository);
  return stable(files[pin.path].match(pin.pattern)[0]);
};

test('current and older upstream releases preserve all files', async () => {
  assert.deepEqual((await propose(files, current)).files, files);
  assert.deepEqual((await propose(files, () => stable('v0.0.0'))).files, files);
});

test('proposes only version changes, preserving action identities and verification', async () => {
  const versions = new Map(pins.map(pin => {
    const parts = current(pin.repo).tag_name.slice(1).split('.');
    parts[2] = String(BigInt(parts[2]) + 1n);
    return [pin.repo, `v${parts.join('.')}`];
  }));
  const result = await propose(files, repo => stable(versions.get(repo)));
  const expected = { ...files };
  for (const pin of pins) expected[pin.path] = expected[pin.path].replace(pin.pattern, versions.get(pin.repo));
  assert.deepEqual(result.files, expected);
  assert.deepEqual(Object.keys(result.files), Object.keys(files));
  assert.match(result.summary, /never updates or bypasses/);
  assert.match(result.files['scripts/third-party-notices.sh'], /GOSUMDB=sum\.golang\.org GOPRIVATE= GONOSUMDB= GONOPROXY= GOPROXY=https:\/\/proxy\.golang\.org/);
  assert.deepEqual(files, Object.fromEntries(pins.map(pin => [pin.path, readFileSync(resolve(root, pin.path), 'utf8')])));
});

test('new go-licenses module major is reported without an invalid v2 path update', async () => {
  const result = await propose(files, repo => repo === 'google/go-licenses' ? stable('v3.0.0') : current(repo));
  assert.deepEqual(result.files, files);
  assert.match(result.summary, /Manual module-major migration required/);
});

test('version precedence is numeric across patch, minor and major boundaries', async () => {
  const pin = pins[0];
  const fixture = { ...files, [pin.path]: files[pin.path].replace(pin.pattern, 'v2.9.99') };
  for (const [tag, expected] of [['v2.10.0', true], ['v3.0.0', true], ['v2.9.100', true], ['v2.8.999', false], ['v1.99.999', false]]) {
    const result = await propose(fixture, repo => repo === pin.repo ? stable(tag) : current(repo));
    assert.equal(result.files[pin.path].match(pin.pattern)[0], expected ? tag : 'v2.9.99');
  }
});

test('rejects draft, prerelease, missing flags and malicious or non-semver release tags', async () => {
  for (const release of [
    { ...stable('v9.0.0'), draft: true }, { ...stable('v9.0.0'), prerelease: true },
    { tag_name: 'v9.0.0' }, stable('v9.0.0-rc1'), stable('v9.0.0\nrun: evil'),
    stable('v9.0.0;echo'), stable('v09.0.0'), stable('latest'), stable(null),
  ]) await assert.rejects(propose(files, () => release));
});

test('missing, duplicate and malformed source pins fail instead of silently skipping', async () => {
  for (const pin of pins) {
    const source = files[pin.path];
    for (const value of ['', source + source, source.replace(pin.pattern, 'vlatest')]) {
      await assert.rejects(propose({ ...files, [pin.path]: value }, current));
    }
  }
});

test('upstream errors propagate with no mutation', async () => {
  const error = new Error('API unavailable');
  await assert.rejects(propose(files, () => { throw error; }), candidate => candidate === error);
  assert.deepEqual(files, Object.fromEntries(pins.map(pin => [pin.path, readFileSync(resolve(root, pin.path), 'utf8')])));
});

test('workflow stays read-only and publishes proposals without running new binaries', () => {
  const workflow = readFileSync(resolve(root, '.github/workflows/build-tool-updates.yml'), 'utf8');
  assert.match(workflow, /permissions:\n  contents: read/);
  assert.doesNotMatch(workflow, /: write|pull_request_target|secrets\.|git push|go install|goreleaser release/);
  assert.match(workflow, /persist-credentials: false/);
  assert.match(workflow, /git diff -- .* > build-tool-updates.patch/);
  assert.match(workflow, /build-tool-updates-base.txt/);
  for (const match of workflow.matchAll(/uses: (\S+)/g)) assert.match(match[1], /@[a-f0-9]{40}$/);
});
