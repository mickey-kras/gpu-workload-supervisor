function inspectReleaseEntry(files, workflows, failures, checks) {
  const { event, step } = checks;
  const release = '.github/workflows/release.yml';
  event(release, 'workflow_dispatch');
  const releaseJobs = workflows[release]?.jobs;
  if (workflows[release]?.on?.workflow_dispatch?.inputs ||
      releaseJobs?.entry?.outputs?.version !== '${{ steps.version.outputs.version }}' ||
      !['quality', 'codeql'].every(id => releaseJobs?.[id]?.needs === 'entry') ||
      JSON.stringify(releaseJobs?.publish?.needs) !== '["entry","quality","codeql"]') {
    failures.push(`${release} lost dispatch version selection`);
  }
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
