const test = require('node:test');
const assert = require('node:assert/strict');
const { classifyFiles, run } = require('./pr-validation-scope.cjs');

const HEAD = 'a'.repeat(40);
const BASE = 'b'.repeat(40);
const file = (filename, status = 'modified') => ({ filename, status,
  sha: HEAD,
  additions: 1, deletions: 1, changes: 2, patch: '@@ -1 +1 @@\n-old\n+new' });

function fixture() {
  const pull = { number: 8, state: 'open', changed_files: 1,
    head: { sha: HEAD, repo: { full_name: 'mickey-kras/gpu-workload-supervisor' } },
    base: { sha: BASE, ref: 'main', repo: { full_name: 'mickey-kras/gpu-workload-supervisor' } } };
  const state = { pull, files: [file('docs/OPERATOR.md')], outputs: [], logs: [], reads: 0 };
  const github = { rest: { pulls: {
    get: async () => { state.reads++; return { data: structuredClone(state.pull) }; }, listFiles() {},
  } }, paginate: async (method, params) => {
    assert.equal(method, github.rest.pulls.listFiles);
    assert.equal(params.per_page, 100);
    return structuredClone(state.files);
  } };
  return { state, github, context: { eventName: 'pull_request', payload: { pull_request: structuredClone(pull) },
    repo: { owner: 'mickey-kras', repo: 'gpu-workload-supervisor' } },
  core: { setOutput: (name, value) => state.outputs.push([name, value]), info: value => state.logs.push(value) } };
}

test('only complete Markdown documentation diffs select docs-only validation', () => {
  for (const path of ['README.md', 'docs/README.md', 'docs/releases/v0.1.0.md']) {
    assert.equal(classifyFiles([file(path)], 1), 'docs-only');
  }
  assert.equal(classifyFiles([file('docs/README.md'), file('README.md')], 2), 'docs-only');
});

test('runtime, package, workflow, dependencies, configuration and mixed paths select full', () => {
  for (const path of ['internal/setup/setup.go', 'cmd/gpu-mode/main.go', 'clients/setup/setup.js',
    'packaging/service.md', '.github/workflows/ci.yml', '.github/scripts/pr-validation-scope.cjs',
    'go.mod', 'go.sum', 'clients/gnome/package-lock.json', '.goreleaser.yaml',
    '.semgrep.yml', 'docs/examples/config.yaml', 'SECURITY.md', 'unknown.md']) {
    assert.equal(classifyFiles([file(path)], 1), 'full', path);
    assert.equal(classifyFiles([file('docs/README.md'), file(path)], 2), 'full', path);
  }
});

test('renames classify both old and new paths; deletions require complete metadata', () => {
  const rename = { ...file('docs/NEW.md', 'renamed'), previous_filename: 'docs/OLD.md' };
  assert.equal(classifyFiles([rename], 1), 'docs-only');
  for (const mutate of [
    f => { f.previous_filename = 'internal/setup/setup.go'; },
    f => { f.filename = 'internal/setup/setup.go'; },
    f => { delete f.previous_filename; },
  ]) {
    const changed = structuredClone(rename); mutate(changed);
    assert.equal(classifyFiles([changed], 1), 'full');
  }
  const removed = { ...file('docs/OLD.md', 'removed'), additions: 0, changes: 1 };
  assert.equal(classifyFiles([removed], 1), 'docs-only');
  delete removed.deletions;
  assert.equal(classifyFiles([removed], 1), 'full');
});

test('unknown statuses, incomplete pages, duplicate files, truncation and invalid paths fail closed', () => {
  for (const files of [[], [null], ['docs/README.md'], [file('docs/README.md'), file('docs/README.md')],
    [{ ...file('docs/README.md'), status: 'copied' }],
    [{ ...file('docs/README.md'), patch: undefined }],
    [{ ...file('docs/README.md'), changes: undefined }],
    [{ ...file('docs/README.md'), sha: undefined }],
    [{ ...file('docs/README.md'), patch: '' }],
    [{ ...file('docs/README.md'), additions: 99 }],
    [file('docs/../internal/setup.md')], [file('/docs/README.md')], [file('docs\\README.md')]]) {
    assert.equal(classifyFiles(files, 1), 'full');
  }
  for (const count of [undefined, 0, 2, 3001, '1']) {
    assert.equal(classifyFiles([file('docs/README.md')], count), 'full');
  }
});

test('read-only classification supports forks and reports the selected mode', async () => {
  const input = fixture(); input.state.pull.head.repo.full_name = 'contributor/fork';
  await run(input);
  assert.deepEqual(input.state.outputs, [['validation-mode', 'docs-only']]);
  assert.equal(input.state.reads, 2);
  assert.match(input.state.logs[0], /complete documentation-only diff/);
});

test('main and release events always select full without diff reads', async () => {
  for (const eventName of ['push', 'workflow_dispatch', 'workflow_call']) {
    const input = fixture(); input.context.eventName = eventName;
    assert.equal(await run(input), 'full');
    assert.equal(input.state.reads, 0);
  }
});

test('mismatched or missing live/event metadata selects full', async () => {
  for (const mutate of [
    input => { input.state.pull.head.sha = BASE; },
    input => { input.state.pull.base.sha = HEAD; },
    input => { input.state.pull.base.ref = 'release/0.1.0'; },
    input => { input.state.pull.state = 'closed'; },
    input => { input.context.payload.pull_request.head.sha = undefined; },
    input => { input.state.pull.base.repo.full_name = 'other/repo'; },
  ]) {
    const input = fixture(); mutate(input);
    assert.equal(await run(input), 'full');
  }
});

test('head/base or total file count changing during diff reads selects full', async () => {
  for (const mutate of [
    pull => { pull.head.sha = BASE; }, pull => { pull.base.sha = HEAD; },
    pull => { pull.changed_files++; },
  ]) {
    const input = fixture(); input.github.paginate = async () => {
      mutate(input.state.pull); return input.state.files;
    };
    assert.equal(await run(input), 'full');
    assert.match(input.state.logs[0], /changed during classification/);
  }
});

test('API failures fall back to full and log a safe reason without payloads', async () => {
  const input = fixture(); input.github.paginate = async () => {
    throw new Error('Bearer secret-token API payload');
  };
  assert.equal(await run(input), 'full');
  assert.match(input.state.logs[0], /unavailable/);
  assert.doesNotMatch(input.state.logs[0], /secret-token|Bearer/);
});
