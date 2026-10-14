const { readFileSync, writeFileSync } = require('node:fs');

// Dependabot updates action refs, not these shell literals or action inputs.
const pins = [
  { repo: 'goreleaser/goreleaser', path: '.github/actions/setup-goreleaser/action.yml', pattern: /(?<=install-goreleaser\.sh )v\S+/g },
  { repo: 'sigstore/cosign', path: '.github/actions/setup-goreleaser/action.yml', pattern: /(?<=cosign-release: )v\S+/g },
  { repo: 'aquasecurity/trivy', path: '.github/workflows/release.yml', pattern: /(?<=          version: )v\S+/g },
  { repo: 'google/go-licenses', path: 'scripts/third-party-notices.sh', pattern: /(?<=github\.com\/google\/go-licenses\/v2@)v\S+/g },
];

function version(tag) {
  if (typeof tag !== 'string' || !/^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/.test(tag)) {
    throw new Error('Expected a stable vMAJOR.MINOR.PATCH tag');
  }
  return tag.slice(1).split('.').map(BigInt);
}

async function propose(files, latest) {
  const updated = { ...files };
  const rows = [];
  for (const pin of pins) {
    const matches = files[pin.path]?.match(pin.pattern);
    if (matches?.length !== 1) throw new Error(`Expected one ${pin.repo} pin in ${pin.path}`);
    const current = matches[0];
    const before = version(current);
    const release = await latest(pin.repo);
    if (release.draft !== false || release.prerelease !== false) {
      throw new Error(`Expected a published stable release for ${pin.repo}`);
    }
    const after = version(release.tag_name);
    const difference = after.findIndex((part, index) => part !== before[index]);
    const newer = difference !== -1 && after[difference] > before[difference];
    let status = 'Current or ahead of latest';
    if (newer) {
      status = 'Proposed';
      if (pin.repo === 'google/go-licenses' && after[0] !== before[0]) {
        status = 'Manual module-major migration required';
      } else {
        updated[pin.path] = updated[pin.path].replace(pin.pattern, release.tag_name);
      }
    }
    rows.push(`| ${pin.repo} | ${current} | [${release.tag_name}](https://github.com/${pin.repo}/releases/tag/${release.tag_name}) | ${status} |`);
  }
  return {
    files: updated,
    summary: [
      '| Tool | Pinned | Latest stable | Status |', '| --- | --- | --- | --- |', ...rows, '',
      'The artifact contains a proposed version-only patch. It has not installed or validated new binaries.',
      'Apply on the recorded base commit, review upstream changes, and run the normal PR gates.',
      'GoReleaser/cosign changes also require an explicitly reviewed installer digest update in policy-guard.cjs; this check never updates or bypasses that protection.',
    ].join('\n') + '\n',
  };
}

async function run({ github, core }) {
  const files = Object.fromEntries(pins.map(pin => [pin.path, readFileSync(pin.path, 'utf8')]));
  const result = await propose(files, async repository => {
    const [owner, repo] = repository.split('/');
    const { data } = await github.rest.repos.getLatestRelease({ owner, repo });
    return data;
  });
  for (const [path, content] of Object.entries(result.files)) writeFileSync(path, content);
  writeFileSync('build-tool-updates.md', result.summary);
  await core.summary.addRaw(result.summary).write();
}

module.exports = { pins, propose, run };
