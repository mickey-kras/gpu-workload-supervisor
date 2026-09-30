'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const { recover } = require('./recover-release.cjs');

function fixture() {
  const state = { deleted: [], release: {
    id: 399632732, immutable: false, draft: false, prerelease: false,
    target_commitish: '101107b34f79440737800441d46b1ac82c5bfe63',
    assets: [{"name":"gpu-workload-supervisor-v0.1.0.tar.gz","digest":"sha256:18c9402549816638a71c0c7b31b2855bf233f14056b1888bd6892f667532c4f3"},{"name":"gpu-workload-supervisor_0.1.0_linux_amd64.tar.gz","digest":"sha256:b4fd822f5d9343abf8f8e41353ffeb09c8201b53fe0d22baef56e524297bb447"},{"name":"gpu-workload-supervisor_0.1.0_linux_arm64.tar.gz","digest":"sha256:4f281c981073b62a03858c6f8587cc102492a16f5ea791a628bd1c30418fd23e"},{"name":"sbom.cdx.json","digest":"sha256:fc9d0b3014315877473b3c7d9bab64fedf5b34c547ccbf29091dd668c3d25c5d"},{"name":"SHA256SUMS","digest":"sha256:1e11f29889c77adfcd50389919fe3f0a149667aa3d6d65dc0503ae3717ce9cf1"}].map(a => ({ ...a, state: 'uploaded' })),
  } };
  const github = { rest: {
    repos: {
      getContent: async () => ({ data: {
        content: Buffer.from('{"version":"0.1.0"}').toString('base64'),
      } }),
      getReleaseByTag: async () => ({ data: state.release }),
      deleteRelease: async () => { state.deleted.push('release'); },
    },
    git: {
      getRef: async () => ({ data: { object: { type: 'commit', sha: '101107b34f79440737800441d46b1ac82c5bfe63' } } }),
      deleteRef: async args => { state.deleted.push(args.ref); },
    },
  } };
  const context = { eventName: 'workflow_dispatch', ref: 'refs/heads/main',
    repo: { owner: 'mickey-kras', repo: 'gpu-workload-supervisor' } };
  return { state, github, context };
}
test('only the exact mutable publication and refs can be removed', async () => {
  const input = fixture();
  input.github.rest.repos.getContent = async () => ({ data: {
    content: Buffer.from(JSON.stringify({ version: '0.1.0' }, null, 2) + '\n').toString('base64'),
  } });
  await recover(input);
  assert.deepEqual(input.state.deleted,
    ['release', 'tags/v0.1.0', 'heads/release/0.1.0']);
});
test('changed release identity, assets, immutable state, and refs fail before deletion', async () => {
  for (const change of [
    input => { input.state.release.immutable = true; },
    input => { input.state.release.id++; },
    input => { input.state.release.assets[0].digest = 'sha256:other'; },
    input => { input.github.rest.git.getRef = async () => ({ data: {
      object: { type: 'commit', sha: 'b'.repeat(40) },
    } }); },
  ]) {
    const input = fixture();
    input.github.rest.repos.getContent = async () => ({ data: {
      content: Buffer.from(JSON.stringify({ version: '0.1.0' }, null, 2) + '\n').toString('base64'),
    } });
    change(input);
    await assert.rejects(recover(input));
    assert.deepEqual(input.state.deleted, []);
  }
});
