function requireValue(condition, message) {
  if (!condition) throw new Error(message);
}

async function verifyPublished({ github, context, version }) {
  requireValue(typeof version === 'string' && version.length <= 256 &&
    /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/.test(version) &&
    version.split('.').every(part => Number.isSafeInteger(Number(part))),
  'release-version.json must contain one plain SemVer version');
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

module.exports = { verifyPublished };
