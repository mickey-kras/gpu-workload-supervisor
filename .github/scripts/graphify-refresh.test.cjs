const test = require('node:test');
const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const { runInNewContext } = require('node:vm');
const YAML = require('yaml');

const workflow = YAML.parse(readFileSync('.github/workflows/graphify.yml', 'utf8'));
const reconcile = workflow.jobs.refresh.steps.find(step => step.name === 'Reconcile the refresh PR');

async function run({ changed = false, moved = false, open = [], failClose = false } = {}) {
  const calls = [];
  const github = {
    rest: {
      git: {
        getRef: async () => ({ data: { object: { sha: moved ? 'new-main' : 'main' } } }),
        getCommit: async () => ({ data: { tree: { sha: 'tree' } } }),
        createBlob: async () => ({ data: { sha: 'blob' } }),
        createTree: async () => ({ data: { sha: 'tree' } }),
        createCommit: async () => ({ data: { sha: 'commit' } }),
        updateRef: async args => { calls.push(['updateRef', args]); },
      },
      pulls: {
        list: 'list',
        update: async args => {
          if (failClose) throw new Error('Close failed');
          calls.push(['close', args]);
        },
        create: async args => {
          calls.push(['create', args]);
          return { data: { number: 8 } };
        },
        get: async () => ({ data: { node_id: 'pull', auto_merge: true } }),
      },
    },
    paginate: async (method, args) => {
      assert.equal(method, 'list');
      assert.equal(args.head, 'owner:ci/graphify-refresh');
      assert.equal(args.state, 'open');
      assert.equal(args.base, 'main');
      return open;
    },
  };
  await runInNewContext(`(async () => { ${reconcile.with.script} })()`, {
    github,
    context: { repo: { owner: 'owner', repo: 'repo' }, sha: 'main' },
    process: { env: { GRAPH: 'graph.json', REFRESH_BRANCH: 'ci/graphify-refresh', GRAPH_CHANGED: String(changed) } },
    require: () => ({ readFileSync: () => Buffer.from('{}') }),
    core: { notice() {}, summary: { addRaw() { return this; }, async write() {} } },
  });
  return calls;
}

test('no-change reconciliation and its App token are not skipped', () => {
  assert.equal(reconcile.if, undefined);
  assert.equal(workflow.jobs.refresh.steps.find(step => step.id === 'app').if, undefined);
  assert.equal(reconcile.env.GRAPH_CHANGED, '${{ steps.build.outputs.changed }}');
});

test('a source revert closes the obsolete refresh PR without publishing a graph', async () => {
  const calls = await run({ open: [{ number: 7, auto_merge: true }] });
  assert.equal(calls.length, 1);
  assert.equal(calls[0][0], 'close');
  assert.equal(calls[0][1].pull_number, 7);
  assert.equal(calls[0][1].state, 'closed');
});

test('an unchanged graph without an open PR does nothing', async () => {
  assert.deepEqual(await run(), []);
});

test('an older run cannot close a refresh PR after main moves', async () => {
  assert.deepEqual(await run({ moved: true, open: [{ number: 7 }] }), []);
});

test('close failures fail the workflow', async () => {
  await assert.rejects(run({ open: [{ number: 7 }], failClose: true }), /Close failed/);
});

test('changed graphs still update the existing refresh branch', async () => {
  const calls = await run({ changed: true, open: [{ number: 7 }] });
  assert.deepEqual(calls.map(([name]) => name), ['updateRef']);
});

test('changed graphs still create a refresh PR when needed', async () => {
  const calls = await run({ changed: true });
  assert.deepEqual(calls.map(([name]) => name), ['updateRef', 'create']);
});
