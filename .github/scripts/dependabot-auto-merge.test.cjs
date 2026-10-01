const test = require('node:test');
const assert = require('node:assert/strict');
const { trustedPull, eligible, readDependencies } = require('./dependabot-auto-merge.cjs');

const bot = { login: 'dependabot[bot]', id: 49699333 };
const pull = {
  user: bot, state: 'open', draft: false,
  head: { ref: 'dependabot/go_modules/sqlite-1.60.1', repo: { full_name: 'owner/repo' } },
  base: { ref: 'main' },
};
const commits = [{ author: bot, commit: { verification: { verified: true } } }];
const update = {
  updateType: 'version-update:semver-patch',
  prevVersion: '1.60.0', newVersion: '1.60.1', compatScore: 75,
};

test('only the exact bot identity and same-repository branch are trusted', () => {
  assert.equal(trustedPull(pull, 'owner/repo', 'main'), true);
  assert.equal(trustedPull({ ...pull, user: { ...bot, id: 1 } }, 'owner/repo', 'main'), false);
  assert.equal(trustedPull({ ...pull, head: { ...pull.head, repo: { full_name: 'fork/repo' } } },
    'owner/repo', 'main'), false);
  assert.equal(trustedPull({ ...pull, draft: true }, 'owner/repo', 'main'), false);
});

test('every update must be minor or patch with a known score of at least 75', () => {
  assert.equal(eligible(commits, [update]), true);
  assert.equal(eligible(commits, [update, { ...update, compatScore: 74 }]), false);
  assert.equal(eligible(commits, [{ ...update, updateType: 'version-update:semver-major' }]), false);
  assert.equal(eligible(commits, [{ ...update, compatScore: null }]), false);
  assert.equal(eligible([], [update]), false);
  assert.equal(eligible([{ ...commits[0], author: { login: 'user', id: 1 } }], [update]), false);
  assert.equal(eligible([{ ...commits[0], commit: { verification: { verified: false } } }], [update]), false);
});

test('metadata parser rejects missing or incomplete action output', () => {
  assert.deepEqual(readDependencies('updated-dependencies-json<<END\n[]\nEND\n'), []);
  assert.throws(() => readDependencies(''), /missing/);
  assert.throws(() => readDependencies('updated-dependencies-json<<END\n[]\n'), /Incomplete/);
});

test('grouped CodeQL metadata preserves per-dependency eligibility checks', () => {
  const dependencies = ['init', 'analyze'].map(action => ({
    ...update, dependencyName: `github/codeql-action/${action}`,
    prevVersion: '4.38.1', newVersion: '4.38.2',
  }));
  const parse = entries => readDependencies(
    `updated-dependencies-json<<END\n${JSON.stringify(entries)}\nEND\n`);
  assert.equal(eligible(commits, parse(dependencies)), true);
  for (const invalid of [{ compatScore: 74 }, { compatScore: null },
    { updateType: 'version-update:semver-major' }]) {
    assert.equal(eligible(commits, parse([dependencies[0], { ...dependencies[1], ...invalid }])), false);
  }
});
