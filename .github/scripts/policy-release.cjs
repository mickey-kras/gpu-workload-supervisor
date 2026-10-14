const { createHash } = require('node:crypto');

const VALIDATION_SCRIPT = [
  "await require('./.github/scripts/release-validation.cjs').verifyMain({",
  '  github, context, core, version: process.env.VERSION,', '});', '',
].join('\n');

function inspectValidationReuse(files, releaseJobs, failures, checks) {
  const path = '.github/workflows/release.yml';
  const validation = releaseJobs?.validation;
  const verification = releaseJobs?.publish?.steps?.find(step =>
    step.name === 'Verify main and successful validation at this commit');
  const digest = content => createHash('sha256').update(content || '').digest('hex');
  if (digest(files['.github/scripts/release-validation.cjs']) !==
      'da0aa5baa3644818864a925255c719bab9de011c7bdfe443f45637aebd4e7af8') {
    failures.push('Release validation verifier was changed');
  }
  if (!validation) {
    for (const id of ['quality', 'aislop', 'codeql']) {
      checks.job(path, id, `./.github/workflows/${id === 'quality' ? 'ci' : id}.yml`);
      if (releaseJobs?.[id]?.needs !== 'entry') failures.push(`${path} lost legacy release ${id} ordering`);
    }
    if (JSON.stringify(releaseJobs?.publish?.needs) !== '["entry","quality","aislop","codeql"]' ||
        digest(verification?.with?.script) !==
        'c6eb1c8ff3d93b28e068a4348b7d3b46b869efdd930f4c3e91b8f1c7a9415a47' ||
        Object.hasOwn(verification || {}, 'if')) {
      failures.push(`${path} lost legacy exact-commit release validation`);
    }
    return;
  }
  if (['quality', 'aislop', 'codeql'].some(id => Object.hasOwn(releaseJobs, id)) ||
      validation.needs !== 'entry' || Object.hasOwn(validation, 'if') ||
      validation['runs-on'] !== 'ubuntu-latest' ||
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
    uses: 'actions/github-script', withValues: { script: VALIDATION_SCRIPT },
  });
  checks.step(path, 'validation', 'Install release tools', {
    run: ['npm ci --prefix .github/scripts --ignore-scripts --no-audit --no-fund'],
  });
  checks.exactRun(path, 'validation', 'Install release tools', [
    'npm ci --prefix .github/scripts --ignore-scripts --no-audit --no-fund',
  ]);
  checks.step(path, 'publish', 'Verify main and successful validation at this commit', {
    uses: 'actions/github-script', withValues: { script: VALIDATION_SCRIPT },
  });
  for (const step of [verification, validation.steps?.find(step =>
    step.name === 'Verify exact-commit main validation')]) {
    if (step?.env?.VERSION !== '${{ needs.entry.outputs.version }}' ||
        Object.hasOwn(step?.with || {}, 'github-token')) {
      failures.push(`${path} changed release validation version or read token`);
    }
  }
}

function inspectReleaseEntry(files, workflows, failures, checks) {
  const { event, step } = checks;
  const release = '.github/workflows/release.yml';
  event(release, 'workflow_dispatch');
  const releaseJobs = workflows[release]?.jobs;
  if (workflows[release]?.on?.workflow_dispatch?.inputs ||
      releaseJobs?.entry?.outputs?.version !== '${{ steps.version.outputs.version }}') {
    failures.push(`${release} lost dispatch version selection`);
  }
  inspectValidationReuse(files, releaseJobs, failures, checks);
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

module.exports = { inspectReleaseEntry, inspectReleasePublish };
