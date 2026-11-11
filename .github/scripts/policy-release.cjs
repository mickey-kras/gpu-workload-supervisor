const { readPolicyConstant } = require('./policy-admission.cjs');
const { createHash } = require('node:crypto');
const { sameActionSteps } = require('./policy-action-pins.cjs');

const RELEASE_HASHES = {
  ".github/scripts/release-validation.cjs": "a0b980507a2b5e4b0a7d27a11b36afd27db5f39dd2d74bca72ab82eecf9ec337",
  ".github/scripts/release-publication.cjs": "124d6832b052c0f3309755c1874acf303bbef264cf24077e1831dacc51838462"
};

// Both release jobs verify the helpers against this one checked-in manifest. The
// runtime gate reads it from the dispatched commit through the API, never from
// the workspace, so a workspace write cannot replace a helper and its digest.
const RELEASE_DIGEST_MANIFEST = '.github/release-helper-digests.json';

const VALIDATION_SCRIPT = [
  "const { readFileSync } = require('node:fs');",
  "const { createHash } = require('node:crypto');",
  "const { resolve } = require('node:path');",
  "const { compileFunction } = require('node:vm');",
  'const { data: manifest } = await github.rest.repos.getContent({',
  `  ...context.repo, path: '${RELEASE_DIGEST_MANIFEST}', ref: context.sha,`, '});',
  "if (manifest?.type !== 'file' || manifest.encoding !== 'base64' || typeof manifest.content !== 'string') {",
  "  throw new Error('Release helper digest manifest is unreadable');", '}',
  "const digests = JSON.parse(Buffer.from(manifest.content, 'base64').toString('utf8'));",
  'const verified = (path, message) => {',
  '  const bytes = readFileSync(resolve(path));',
  "  const expected = Object.hasOwn(digests, path) ? digests[path] : '';",
  "  if (!/^[a-f0-9]{64}$/.test(expected) || createHash('sha256').update(bytes).digest('hex') !== expected) {",
  '    throw new Error(message);', '  }',
  '  return bytes;', '};',
  "const filename = resolve('.github/scripts/release-validation.cjs');",
  "const source = verified('.github/scripts/release-validation.cjs',",
  "  'Release validation verifier bytes changed in the workspace');",
  "const publicationFilename = resolve('.github/scripts/release-publication.cjs');",
  "const publicationSource = verified('.github/scripts/release-publication.cjs',",
  "  'Release publication helper bytes changed in the workspace');",
  'const publication = { exports: {} };',
  "const noImports = () => { throw new Error('Unexpected publication helper dependency'); };",
  "compileFunction(publicationSource.toString('utf8'), ['require', 'module', 'exports'],",
  '  { filename: publicationFilename })(noImports, publication, publication.exports);',
  'const closedRequire = name => {',
  "  if (name !== './release-publication.cjs') throw new Error('Unexpected validation dependency');",
  '  return publication.exports;', '};',
  'const validated = { exports: {} };',
  "compileFunction(source.toString('utf8'), ['require', 'module', 'exports'], { filename })(",
  '  closedRequire, validated, validated.exports,', ');',
  'await validated.exports.verifyMain({',
  '  github, context, core, version: process.env.VERSION,', '});', '',
].join('\n');

const HARDEN = { uses: 'step-security/harden-runner@351661ca32ac09a36dc5ee2d536e3128f2a3c8ed',
  with: { 'egress-policy': 'audit' } };
const CHECKOUT = { uses: 'actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1',
  with: { ref: '${{ github.sha }}', 'persist-credentials': false } };
const GO = { uses: 'actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e',
  with: { 'go-version-file': 'go.mod', cache: true } };
const INSTALL = { name: 'Install release tools',
  run: 'npm ci --prefix .github/scripts --ignore-scripts --no-audit --no-fund' };
const verificationStep = (name, script) => ({ name,
  uses: 'actions/github-script@3a2844b7e9c422d3c10d287c895573f7108da1b3',
  env: { VERSION: '${{ needs.entry.outputs.version }}' }, with: { script } });

function inspectVerificationWorkspace(workflow, failures, script) {
  const jobs = workflow.jobs;
  const validation = jobs.validation;
  const publish = jobs.publish;
  const sameSteps = sameActionSteps;
  const allowed = ['name', 'needs', 'runs-on', 'timeout-minutes', 'permissions', 'steps'];
  const publishCheckout = { ...CHECKOUT, with: { 'fetch-depth': 0, ...CHECKOUT.with } };
  const beforePublish = publish.steps?.findIndex(step =>
    step.name === 'Verify main and successful validation at this commit');
  if (Object.hasOwn(workflow, 'env') || Object.hasOwn(workflow, 'defaults') ||
      Object.keys(validation).some(key => !allowed.includes(key)) ||
      publish['runs-on'] !== 'ubuntu-24.04' ||
      ['env', 'defaults', 'container', 'services'].some(key => Object.hasOwn(publish, key)) ||
      !sameSteps(validation.steps, [HARDEN, CHECKOUT,
        verificationStep('Verify exact-commit main validation', script)]) ||
      !sameSteps(publish.steps?.slice(0, beforePublish + 1), [HARDEN, publishCheckout, GO, INSTALL,
        verificationStep('Verify main and successful validation at this commit', script)])) {
    failures.push('Release validation workspace or runtime inputs were changed');
  }
}

// The runtime manifest must list exactly the reviewed policy digests.
function inspectDigestManifest(files, expected, failures) {
  let manifest;
  try {
    manifest = JSON.parse(files[RELEASE_DIGEST_MANIFEST]);
  } catch {
    manifest = undefined;
  }
  const keys = Object.keys(expected).sort();
  if (!manifest || typeof manifest !== 'object' || Array.isArray(manifest) ||
      JSON.stringify(Object.keys(manifest).sort()) !== JSON.stringify(keys) ||
      keys.some(key => typeof expected[key] !== 'string' || manifest[key] !== expected[key])) {
    failures.push(`${RELEASE_DIGEST_MANIFEST} does not match the reviewed release helper digests`);
  }
}

function inspectValidationReuse(files, workflow, failures, checks, admitted) {
  const releaseJobs = workflow?.jobs;
  const path = '.github/workflows/release.yml';
  const validation = releaseJobs?.validation;
  const verification = releaseJobs?.publish?.steps?.find(step =>
    step.name === 'Verify main and successful validation at this commit');
  const script = VALIDATION_SCRIPT;
  let candidateHashes = {};
  if (Object.keys(RELEASE_HASHES).some(file => admitted.has(file))) {
    try {
      const source = files['.github/scripts/policy-release.cjs'] || '';
      candidateHashes = readPolicyConstant(source, 'RELEASE_HASHES');
    } catch {
      failures.push('Release helper digests are invalid');
    }
  }
  const expectedDigests = {};
  for (const [file, hash] of Object.entries(RELEASE_HASHES)) {
    const actual = createHash('sha256').update(files[file] || '').digest('hex');
    const expected = admitted.has(file) ? candidateHashes?.[file] : hash;
    if (actual !== expected) failures.push(file.endsWith('release-validation.cjs')
      ? 'Release validation verifier was changed' : 'Release publication helper was changed');
    expectedDigests[file] = expected;
  }
  inspectDigestManifest(files, expectedDigests, failures);
  if (!validation) {
    failures.push(`${path} lost exact-commit main validation`);
    return;
  }
  inspectVerificationWorkspace(workflow, failures, script);
  if (['quality', 'aislop', 'codeql'].some(id => Object.hasOwn(releaseJobs, id)) ||
      validation.needs !== 'entry' || Object.hasOwn(validation, 'if') ||
      validation['runs-on'] !== 'ubuntu-24.04' ||
      validation['timeout-minutes'] !== 75 ||
      JSON.stringify(validation.permissions) !== '{"contents":"read","actions":"read"}' ||
      JSON.stringify(releaseJobs?.publish?.needs) !== '["entry","validation"]') {
    failures.push(`${path} lost exact-commit main validation ordering or permissions`);
  }
  const checkout = validation.steps?.find(step => step.uses?.startsWith('actions/checkout@'));
  if (checkout?.with?.ref !== '${{ github.sha }}' || checkout.with['persist-credentials'] !== false ||
      Object.hasOwn(checkout, 'if')) {
    failures.push(`${path} lost exact-dispatch validation checkout`);
  }
  checks.step(path, 'validation', 'Verify exact-commit main validation', {
    uses: 'actions/github-script', withValues: { script },
  });
  checks.step(path, 'publish', 'Verify main and successful validation at this commit', {
    uses: 'actions/github-script', withValues: { script },
  });
  for (const step of [verification, validation.steps?.find(step =>
    step.name === 'Verify exact-commit main validation')]) {
    if (step?.env?.VERSION !== '${{ needs.entry.outputs.version }}' ||
        Object.hasOwn(step?.with || {}, 'github-token')) {
      failures.push(`${path} changed release validation version or read token`);
    }
  }
}

function inspectReleaseEntry(files, workflows, failures, checks, admitted = new Set()) {
  const { event, step } = checks;
  const release = '.github/workflows/release.yml';
  event(release, 'workflow_dispatch');
  const releaseJobs = workflows[release]?.jobs;
  if (workflows[release]?.on?.workflow_dispatch?.inputs ||
      releaseJobs?.entry?.outputs?.version !== '${{ steps.version.outputs.version }}') {
    failures.push(`${release} lost dispatch version selection`);
  }
  inspectValidationReuse(files, workflows[release], failures, checks, admitted);
  step(release, 'entry', 'Require dispatch from main', {
    uses: 'actions/github-script',
    withValues: { script: [
      "if (context.eventName !== 'workflow_dispatch' || context.ref !== 'refs/heads/main') {",
      "  throw new Error('Release dispatch is allowed only from main');", '}', '',
    ].join('\n') },
  });
  step(release, 'entry', 'Read planned release version', {
    uses: 'actions/github-script',
    withValues: { script: [
      "const { readFileSync } = require('node:fs');",
      "const { releaseVersion } = require('./.github/scripts/release-follow-up.cjs');",
      "core.setOutput('version', releaseVersion(readFileSync('release-version.json', 'utf8')));", '',
    ].join('\n') },
  });
  if (JSON.stringify(releaseJobs?.['release-followup']?.needs) !== '["entry","publish"]') {
    failures.push(`${release} lost post-publication version bump ordering`);
  }
  step(release, 'release-followup', 'Queue narrowly validated next patch bump', {
    uses: 'actions/github-script',
    withValues: {
      'github-token': '${{ steps.app.outputs.token }}',
      script: [
        "await require('./.github/scripts/release-follow-up.cjs').bumpReleasedVersion({",
        '  github, context, core, version: process.env.VERSION,', '});', '',
      ].join('\n'),
    },
  });
  const bump = releaseJobs?.['release-followup']?.steps?.find(s => s.name === 'Queue narrowly validated next patch bump');
  if (bump?.env?.VERSION !== '${{ needs.entry.outputs.version }}') {
    failures.push(`${release} changed the published bump version`);
  }
  try {
    const planned = JSON.parse(files['release-version.json']);
    if (JSON.stringify(Object.keys(planned)) !== '["version"]' ||
        typeof planned.version !== 'string' ||
        !/^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/.test(planned.version)) {
      failures.push('Invalid planned release version');
    }
  } catch {
    failures.push('Invalid planned release version');
  }
}

function inspectReleasePublish(files, workflows, failures, checks, inspectGoReleaser) {
  const { step } = checks;
  const release = '.github/workflows/release.yml';
  step(release, 'publish', 'Release App token', { uses: 'actions/create-github-app-token' });
  const state = workflows[release]?.jobs?.publish?.steps?.find(s => s.name === 'Verify immutable setting and publication state');
  const verification = [
    "await require('./.github/scripts/release-settings.cjs').verify({",
    '  github, context, review: process.env.RELEASE_SETTINGS_REVIEW,',
    '});',
  ].join('\n');
  if (state?.env?.RELEASE_SETTINGS_REVIEW !== '${{ vars.RELEASE_SETTINGS_REVIEW }}' ||
      !state.with?.script?.startsWith(verification) ||
      state.with?.['github-token'] !== '${{ steps.app.outputs.token }}' ||
      Object.hasOwn(state, 'if') || Object.hasOwn(workflows[release]?.jobs?.publish || {}, 'if')) {
    failures.push(`${release} lost reviewed release settings verification`);
  }
  inspectGoReleaser({ files, workflows, failures, checks, path: release, jobId: 'publish', builds: [
    ['Build deployable binaries', 'release --clean --skip=publish'],
  ], expectedIf: "steps.state.outputs.published != 'true'" });
  step(release, 'publish', 'Build source archive and stage binaries', {
    run: ['git archive', 'test -s "dist/$artifact"', 'cp "dist/$artifact"'],
    expectedIf: "steps.state.outputs.published != 'true'",
  });
  step(release, 'publish', 'Build desktop packages', {
    run: ['for arch in amd64 arm64; do',
      'artifact="gpu-workload-supervisor_${VERSION}_linux_${arch}.deb"',
      'test -s "dist/$artifact"', 'bash scripts/check-desktop-package.sh "dist/$artifact"',
      'cp "dist/$artifact" "release-assets/$artifact"'],
    expectedIf: "steps.state.outputs.published != 'true'",
  });
  checks.exactRun(release, 'publish', 'Build desktop packages', [
    'set -euo pipefail', 'for arch in amd64 arm64; do',
    'artifact="gpu-workload-supervisor_${VERSION}_linux_${arch}.deb"',
    'test -s "dist/$artifact"', 'bash scripts/check-desktop-package.sh "dist/$artifact"',
    'cp "dist/$artifact" "release-assets/$artifact"', 'done',
  ]);
  step(release, 'publish', 'Checksum all release assets', {
    run: ['sha256sum gpu-workload-supervisor*.tar.gz sbom.cdx.json > SHA256SUMS',
      'sha256sum gpu-workload-supervisor*.deb >> SHA256SUMS', 'sha256sum --check SHA256SUMS'],
    expectedIf: "steps.state.outputs.published != 'true'",
  });
  step(release, 'publish', 'Attest release assets', {
    withValues: { 'subject-path': 'release-assets/*.tar.gz\nrelease-assets/*.deb\nrelease-assets/SHA256SUMS\nrelease-assets/sbom.cdx.json\n' },
    uses: 'actions/attest-build-provenance', expectedIf: "steps.state.outputs.published != 'true'",
  });
  step(release, 'publish', 'Publish immutable GitHub release', {
    run: ['gh release create'], expectedIf: "steps.state.outputs.published != 'true'",
  });
}

module.exports = { inspectReleaseEntry, inspectReleasePublish, VALIDATION_SCRIPT, RELEASE_DIGEST_MANIFEST };
