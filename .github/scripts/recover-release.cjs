'use strict';

const assert = require('node:assert/strict');

const EXPECTED_SHA = '101107b34f79440737800441d46b1ac82c5bfe63';
const EXPECTED_RELEASE_ID = 399632732;
const EXPECTED_ASSETS = {
  "gpu-workload-supervisor-v0.1.0.tar.gz": "sha256:18c9402549816638a71c0c7b31b2855bf233f14056b1888bd6892f667532c4f3",
  "gpu-workload-supervisor_0.1.0_linux_amd64.tar.gz": "sha256:b4fd822f5d9343abf8f8e41353ffeb09c8201b53fe0d22baef56e524297bb447",
  "gpu-workload-supervisor_0.1.0_linux_arm64.tar.gz": "sha256:4f281c981073b62a03858c6f8587cc102492a16f5ea791a628bd1c30418fd23e",
  "sbom.cdx.json": "sha256:fc9d0b3014315877473b3c7d9bab64fedf5b34c547ccbf29091dd668c3d25c5d",
  "SHA256SUMS": "sha256:1e11f29889c77adfcd50389919fe3f0a149667aa3d6d65dc0503ae3717ce9cf1"
};

async function recover({ github, context }) {
  assert.equal(context.eventName, 'workflow_dispatch', 'Run recovery manually');
  assert.equal(context.ref, 'refs/heads/main', 'Run recovery from main');
  const repo = context.repo;
  const { data: version } = await github.rest.repos.getContent({
    ...repo, path: 'release-version.json', ref: 'main',
  });
  assert.equal(Buffer.from(version.content, 'base64').toString('utf8'),
    '{\n  "version": "0.1.0"\n}\n', 'The planned release version changed');
  const { data: release } = await github.rest.repos.getReleaseByTag({
    ...repo, tag: 'v0.1.0',
  });
  assert.equal(release.id, EXPECTED_RELEASE_ID, 'Release identity changed');
  assert.equal(release.immutable, false, 'Do not delete an immutable release');
  assert.equal(release.draft, false, 'Release state changed');
  assert.equal(release.prerelease, false, 'Release state changed');
  assert.equal(release.target_commitish, EXPECTED_SHA, 'Release target changed');
  assert.equal(release.assets.length, Object.keys(EXPECTED_ASSETS).length, 'Assets changed');
  assert.deepEqual(Object.fromEntries(release.assets.map(a => [a.name, a.digest])),
    EXPECTED_ASSETS, 'Release assets changed');
  for (const asset of release.assets) assert.equal(asset.state, 'uploaded', 'Asset state changed');
  for (const ref of ['tags/v0.1.0', 'heads/release/0.1.0']) {
    const { data } = await github.rest.git.getRef({ ...repo, ref });
    assert.equal(data.object.type, 'commit', 'Unexpected ref type');
    assert.equal(data.object.sha, EXPECTED_SHA, 'Release ref changed');
  }

  // Repository release immutability must be enabled in Settings before running this workflow.
  await github.rest.repos.deleteRelease({ ...repo, release_id: EXPECTED_RELEASE_ID });
  await github.rest.git.deleteRef({ ...repo, ref: 'tags/v0.1.0' });
  await github.rest.git.deleteRef({ ...repo, ref: 'heads/release/0.1.0' });
}

module.exports = { recover };
