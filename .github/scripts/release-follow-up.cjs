const { execFileSync } = require('node:child_process');
const { readFileSync } = require('node:fs');
const { isDeepStrictEqual } = require('node:util');
const semver = require('semver');

function requireValue(condition, message) {
  if (!condition) throw new Error(message);
}

function releaseVersion(content) {
  const value = JSON.parse(content);
  requireValue(value && isDeepStrictEqual(Object.keys(value), ['version']) &&
    typeof value.version === 'string' && semver.valid(value.version) === value.version &&
    semver.prerelease(value.version) === null && !value.version.includes('+'),
  'release-version.json must contain one plain SemVer version');
  return value.version;
}

function versionFile(version) {
  return `${JSON.stringify({ version }, null, 2)}\n`;
}

async function verifyPublished({ github, context, version }) {
  releaseVersion(versionFile(version));
  requireValue(/^[a-f0-9]{40}$/.test(context.sha), 'Invalid release commit');
  const tag = `v${version}`;
  const { data: ref } = await github.rest.git.getRef({ ...context.repo, ref: `tags/${tag}` });
  const { data: release } = await github.rest.repos.getReleaseByTag({ ...context.repo, tag });
  requireValue(release.immutable === true,
    `Release ${tag} is not verified immutable; preserve its tag and assets, confirm repository release immutability, and reserve an unused version through a normal PR`);
  const expected = [`gpu-workload-supervisor-${tag}.tar.gz`,
    `gpu-workload-supervisor_${version}_linux_amd64.tar.gz`,
    `gpu-workload-supervisor_${version}_linux_arm64.tar.gz`,
    `gpu-workload-supervisor_${version}_linux_amd64.deb`,
    `gpu-workload-supervisor_${version}_linux_arm64.deb`, 'SHA256SUMS', 'sbom.cdx.json'];
  requireValue(ref.object.type === 'commit' && ref.object.sha === context.sha &&
    release.tag_name === tag && !release.draft && !release.prerelease &&
    release.assets.length === expected.length &&
    expected.every(name => release.assets.some(asset => asset.name === name &&
      asset.state === 'uploaded' && /^sha256:[a-f0-9]{64}$/.test(asset.digest || ''))),
  'Published release does not match the immutable candidate');
}

function mergeVersionBump({ repository, number, sha }) {
  execFileSync('gh', ['pr', 'merge', String(number), '--repo', repository,
    '--auto', '--squash', '--match-head-commit', sha], { timeout: 30000, stdio: 'pipe' });
}

async function queueVersionBump({ github, repository, number, branch, version, next, merge }) {
  const params = { ...repository, pull_number: number };
  const { data: pull } = await github.rest.pulls.get(params);
  const fullName = `${repository.owner}/${repository.repo}`;
  requireValue(pull.state === 'open' && !pull.draft && pull.base.ref === 'main' &&
    pull.base.repo?.full_name === fullName && pull.head.repo?.full_name === fullName &&
    pull.head.ref === branch && /^[a-f0-9]{40}$/.test(pull.head.sha),
  `Refusing auto-merge: #${number} is not the expected bump PR`);
  const files = await github.paginate(github.rest.pulls.listFiles, { ...params, per_page: 100 });
  requireValue(files.length === 1 && files[0].filename === 'release-version.json' &&
    !files[0].previous_filename && files[0].status === 'modified' &&
    files[0].additions === 1 && files[0].deletions === 1 &&
    isDeepStrictEqual((files[0].patch || '').split('\n').filter(line => /^[+-]/.test(line)),
      [`-  "version": "${version}"`, `+  "version": "${next}"`]),
  `Refusing auto-merge: #${number} contains changes beyond the next patch version`);
  const { data: file } = await github.rest.repos.getContent({
    ...repository, path: 'release-version.json', ref: pull.head.sha,
  });
  requireValue(file.type === 'file' && file.encoding === 'base64' &&
    Buffer.from(file.content, 'base64').toString('utf8') === versionFile(next),
  `Refusing auto-merge: #${number} has unexpected version contents`);
  const { data: current } = await github.rest.pulls.get(params);
  requireValue(current.head.sha === pull.head.sha && current.state === 'open' && !current.draft &&
    current.base.ref === 'main', `Refusing auto-merge: #${number} changed during validation`);
  if (current.auto_merge) {
    requireValue(current.auto_merge.merge_method === 'squash', `#${number} must use squash auto-merge`);
    return;
  }
  await merge({ repository: fullName, number, sha: pull.head.sha });
}

async function optionalRef(github, repository, ref) {
  try { return (await github.rest.git.getRef({ ...repository, ref })).data; }
  catch (error) {
    if (error.status !== 404) throw error;
    return null;
  }
}

async function bumpReleasedVersion({ github, context, core, version,
  read = () => readFileSync('release-version.json', 'utf8'), merge = mergeVersionBump }) {
  requireValue(context.eventName === 'workflow_dispatch' && context.ref === 'refs/heads/main',
    'Release follow-up requires a dispatch from main');
  requireValue(releaseVersion(read()) === version, 'Released version differs from the dispatch snapshot');
  await verifyPublished({ github, context, version });
  const next = semver.inc(version, 'patch');
  requireValue(next && semver.gt(next, version), 'Cannot increment the released patch version');
  const repository = context.repo;
  const branch = `ci/bump-release-version-${next.replaceAll('.', '-')}`;
  const open = await github.paginate(github.rest.pulls.list, {
    ...repository, state: 'open', head: `${repository.owner}:${branch}`, per_page: 100,
  });
  requireValue(open.length <= 1, 'Multiple version bump PRs found');
  let number = open[0]?.number;
  if (!number) {
    const { data: main } = await github.rest.git.getRef({ ...repository, ref: 'heads/main' });
    const { data: file } = await github.rest.repos.getContent({
      ...repository, path: 'release-version.json', ref: main.object.sha,
    });
    requireValue(file.type === 'file' && file.encoding === 'base64', 'Cannot read the main release version');
    const current = releaseVersion(Buffer.from(file.content, 'base64').toString('utf8'));
    if (current !== version) {
      requireValue(semver.gt(current, version), 'Main version must advance beyond the published version');
      await core.summary.addRaw(`Main already targets ${current}; no bump needed.\n`).write();
      return;
    }
    requireValue(Buffer.from(file.content, 'base64').toString('utf8') === versionFile(version),
      'Main release-version.json must use the canonical version format');
    const { data: base } = await github.rest.git.getCommit({ ...repository, commit_sha: main.object.sha });
    const { data: tree } = await github.rest.git.createTree({
      ...repository, base_tree: base.tree.sha,
      tree: [{ path: 'release-version.json', mode: '100644', type: 'blob', content: versionFile(next) }],
    });
    const ref = `heads/${branch}`;
    const existing = await optionalRef(github, repository, ref);
    if (existing) {
      const { data: commit } = await github.rest.git.getCommit({
        ...repository, commit_sha: existing.object.sha,
      });
      requireValue(existing.object.type === 'commit' && commit.tree.sha === tree.sha &&
        commit.parents.length === 1 && commit.parents[0].sha === main.object.sha,
      'Existing bump branch has unexpected changes; refusing to overwrite it');
    } else {
      const { data: commit } = await github.rest.git.createCommit({
        ...repository, message: `Bump release version to ${next}`, tree: tree.sha, parents: [main.object.sha],
      });
      await github.rest.git.createRef({ ...repository, ref: `refs/${ref}`, sha: commit.sha });
    }
    const { data: pull } = await github.rest.pulls.create({
      ...repository, title: `Bump release version to ${next}`, head: branch, base: 'main',
      body: `Release v${version} is published; reserve ${next} on main.\n\nSquash-merges automatically after required checks pass.\n`,
      maintainer_can_modify: false,
    });
    number = pull.number;
  }
  await queueVersionBump({ github, repository, number, branch, version, next, merge });
  await core.summary.addRaw(`Version bump #${number}: ${version} to ${next}; squash auto-merge enabled.\n`).write();
}

module.exports = { releaseVersion, verifyPublished, bumpReleasedVersion };
