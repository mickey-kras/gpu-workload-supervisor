const test = require('node:test');
const assert = require('node:assert/strict');
const { releaseVersion, verifyPublished, bumpReleasedVersion } = require('./release-follow-up.cjs');

const RELEASE_SHA = 'a'.repeat(40);
const MAIN_SHA = 'b'.repeat(40);
const BUMP_SHA = 'c'.repeat(40);
const TREE_SHA = 'd'.repeat(40);
const VERSION = '0.1.0';
const NEXT = '0.1.1';
const json = version => `${JSON.stringify({ version }, null, 2)}\n`;
const file = version => ({ type: 'file', encoding: 'base64',
  content: Buffer.from(json(version)).toString('base64') });

function fixture() {
  const fullName = 'mickey-kras/gpu-workload-supervisor';
  const context = { eventName: 'workflow_dispatch', ref: 'refs/heads/main', sha: RELEASE_SHA,
    repo: { owner: 'mickey-kras', repo: 'gpu-workload-supervisor' } };
  const release = { tag_name: `v${VERSION}`, immutable: true, draft: false, prerelease: false,
    assets: [`gpu-workload-supervisor-v${VERSION}.tar.gz`,
      `gpu-workload-supervisor_${VERSION}_linux_amd64.tar.gz`,
      `gpu-workload-supervisor_${VERSION}_linux_arm64.tar.gz`, 'SHA256SUMS', 'sbom.cdx.json']
      .map(name => ({ name, state: 'uploaded', digest: `sha256:${'e'.repeat(64)}` })) };
  const pull = { number: 8, state: 'open', draft: false, auto_merge: null,
    base: { ref: 'main', repo: { full_name: fullName } },
    head: { ref: 'ci/bump-release-version-0-1-1', sha: BUMP_SHA, repo: { full_name: fullName } } };
  const files = [{ filename: 'release-version.json', status: 'modified', additions: 1, deletions: 1,
    patch: `@@ -1,3 +1,3 @@\n {\n-  "version": "${VERSION}"\n+  "version": "${NEXT}"\n }` }];
  const state = { mainVersion: VERSION, open: [], existing: null, files, pull, release,
    created: [], merged: [], refs: [], orphanTree: TREE_SHA, parent: MAIN_SHA, summaries: [] };
  const data = value => ({ data: value });
  const github = { paginate: async (method) => {
    if (method === github.rest.pulls.list) return state.open;
    if (method === github.rest.pulls.listFiles) return state.files;
    throw new Error('Unexpected pagination');
  }, rest: {
    repos: {
      getReleaseByTag: async () => data(state.release),
      getContent: async ({ ref }) => data(ref === BUMP_SHA ? file(NEXT) : file(state.mainVersion)),
    },
    git: {
      getRef: async ({ ref }) => {
        if (ref === `tags/v${VERSION}`) return data({ object: { type: 'commit', sha: RELEASE_SHA } });
        if (ref === 'heads/main') return data({ object: { type: 'commit', sha: MAIN_SHA } });
        if (state.existing) return data(state.existing);
        throw Object.assign(new Error('Missing ref'), { status: 404 });
      },
      getCommit: async ({ commit_sha }) => data({ tree: { sha: commit_sha === MAIN_SHA ? 'f'.repeat(40) : state.orphanTree },
        parents: [{ sha: state.parent }] }),
      createTree: async params => { state.created.push(params); return data({ sha: TREE_SHA }); },
      createCommit: async params => { state.created.push(params); return data({ sha: BUMP_SHA }); },
      createRef: async params => { state.refs.push(params); return data({}); },
    },
    pulls: {
      list() {}, listFiles() {}, get: async () => data(structuredClone(state.pull)),
      create: async params => { state.created.push(params); return data(state.pull); },
    },
  } };
  const summary = { addRaw(value) { state.summaries.push(value); return this; }, async write() {} };
  return { github, context, core: { summary }, version: VERSION, read: () => json(VERSION),
    merge: async args => { state.merged.push(args); }, state };
}

test('reads a plain planned version and rejects invalid release metadata', () => {
  assert.equal(releaseVersion(json(VERSION)), VERSION);
  for (const version of ['v0.1.0', '01.1.0', '0.1', '0.1.0-beta', '0.1.0+build', '', 1]) {
    assert.throws(() => releaseVersion(json(version)));
  }
  assert.throws(() => releaseVersion('{"version":"0.1.0","extra":true}'));
  assert.throws(() => releaseVersion('null'));
});

test('published release must be immutable, complete, stable, and at the dispatched SHA', async () => {
  for (const mutate of [
    input => { input.state.release.draft = true; },
    input => { input.state.release.prerelease = true; },
    input => { input.state.release.assets.pop(); },
    input => { input.state.release.assets[0].digest = null; },
    input => { input.context.sha = MAIN_SHA; },
  ]) {
    const input = fixture(); mutate(input);
    await assert.rejects(bumpReleasedVersion(input), /immutable candidate/);
    assert.equal(input.state.created.length, 0);
    assert.equal(input.state.merged.length, 0);
  }
  await verifyPublished(fixture());
});

test('unverified immutable releases cannot queue a bump and explain nondestructive recovery', async () => {
  for (const immutable of [false, undefined, 'true']) {
    const input = fixture();
    input.state.release.immutable = immutable;
    await assert.rejects(bumpReleasedVersion(input),
      /preserve its tag and assets, confirm repository release immutability, and reserve an unused version through a normal PR/);
    assert.equal(input.state.created.length, 0);
    assert.equal(input.state.refs.length, 0);
    assert.equal(input.state.merged.length, 0);
  }
});

test('creates only the next version file and queues squash merge for its exact head', async () => {
  const input = fixture();
  await bumpReleasedVersion(input);
  assert.deepEqual(input.state.created[0].tree,
    [{ path: 'release-version.json', mode: '100644', type: 'blob', content: json(NEXT) }]);
  assert.deepEqual(input.state.merged, [{ repository: 'mickey-kras/gpu-workload-supervisor', number: 8, sha: BUMP_SHA }]);
});

test('reuses the verified open bump PR without writing a second commit', async () => {
  const input = fixture(); input.state.open = [{ number: 8 }];
  await bumpReleasedVersion(input);
  assert.equal(input.state.created.length, 0);
  assert.equal(input.state.merged.length, 1);
});

test('preserves a newer planned version and rejects a main version rollback', async () => {
  const input = fixture(); input.state.mainVersion = '0.2.0';
  await bumpReleasedVersion(input);
  assert.equal(input.state.created.length, 0);
  assert.equal(input.state.merged.length, 0);
  input.state.mainVersion = '0.0.9';
  await assert.rejects(bumpReleasedVersion(input), /must advance/);
});

test('extra files, renamed files, wrong versions, and missing patches cannot auto-merge', async () => {
  for (const mutate of [
    input => { input.state.files.push({ filename: 'README.md' }); },
    input => { input.state.files[0].previous_filename = 'other.json'; },
    input => { input.state.files[0].patch = input.state.files[0].patch.replace(NEXT, '0.2.0'); },
    input => { input.state.files[0].patch = undefined; },
    input => { input.state.files[0].additions = 2; },
    input => { input.state.files[0].status = 'added'; },
  ]) {
    const input = fixture(); input.state.open = [{ number: 8 }]; mutate(input);
    await assert.rejects(bumpReleasedVersion(input), /beyond the next patch/);
    assert.equal(input.state.merged.length, 0);
  }
});

test('foreign heads, changed targets, drafts, and closed PRs cannot auto-merge', async () => {
  for (const mutate of [
    input => { input.state.pull.head.repo.full_name = 'attacker/fork'; },
    input => { input.state.pull.base.ref = 'release/0.1.0'; },
    input => { input.state.pull.head.ref = 'ci/unrelated'; },
    input => { input.state.pull.draft = true; },
    input => { input.state.pull.state = 'closed'; },
  ]) {
    const input = fixture(); input.state.open = [{ number: 8 }]; mutate(input);
    await assert.rejects(bumpReleasedVersion(input), /not the expected bump/);
    assert.equal(input.state.merged.length, 0);
  }
});

test('missing or noncanonical head content prevents auto-merge', async () => {
  const input = fixture(); input.state.open = [{ number: 8 }];
  input.github.rest.repos.getContent = async () => ({ data: file('0.2.0') });
  await assert.rejects(bumpReleasedVersion(input), /unexpected version contents/);
  assert.equal(input.state.merged.length, 0);
});

test('a PR head changing during validation prevents auto-merge', async () => {
  const input = fixture(); input.state.open = [{ number: 8 }];
  let reads = 0;
  input.github.rest.pulls.get = async () => ({ data: { ...structuredClone(input.state.pull),
    head: { ...input.state.pull.head, sha: ++reads === 1 ? BUMP_SHA : MAIN_SHA } } });
  await assert.rejects(bumpReleasedVersion(input), /changed during validation/);
  assert.equal(input.state.merged.length, 0);
});

test('already queued squash merge is idempotent and another merge method is rejected', async () => {
  const input = fixture(); input.state.open = [{ number: 8 }];
  input.state.pull.auto_merge = { merge_method: 'squash' };
  await bumpReleasedVersion(input);
  assert.equal(input.state.merged.length, 0);
  input.state.pull.auto_merge.merge_method = 'merge';
  await assert.rejects(bumpReleasedVersion(input), /must use squash/);
});

test('a validated orphan bump branch is reused and altered branches are preserved', async () => {
  const input = fixture(); input.state.existing = { object: { type: 'commit', sha: BUMP_SHA } };
  await bumpReleasedVersion(input);
  assert.equal(input.state.refs.length, 0);
  assert.equal(input.state.merged.length, 1);
  input.state.orphanTree = MAIN_SHA;
  await assert.rejects(bumpReleasedVersion(input), /refusing to overwrite/);
});

test('API permission failures and auto-merge failures propagate for retry', async () => {
  const input = fixture();
  input.github.rest.git.getRef = async () => { throw Object.assign(new Error('Forbidden'), { status: 403 }); };
  await assert.rejects(bumpReleasedVersion(input), /Forbidden/);
  const retry = fixture(); retry.state.open = [{ number: 8 }];
  retry.merge = async () => { throw new Error('Auto-merge unavailable'); };
  await assert.rejects(bumpReleasedVersion(retry), /Auto-merge unavailable/);
});

test('non-main execution and a changed snapshot version cannot perform follow-up', async () => {
  const input = fixture(); input.context.ref = 'refs/heads/ci/test';
  await assert.rejects(bumpReleasedVersion(input), /dispatch from main/);
  input.context.ref = 'refs/heads/main'; input.read = () => json(NEXT);
  await assert.rejects(bumpReleasedVersion(input), /dispatch snapshot/);
  assert.equal(input.state.created.length, 0);
});
