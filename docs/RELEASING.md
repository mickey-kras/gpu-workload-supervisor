# Source and binary releases

[Documentation](README.md) | [Repository](../README.md)

The release workflow publishes the tested source tree, two Linux archives
(amd64 and arm64), and one Debian package per architecture. Each archive
contains four executables:

- `gpu-mode`: controller; runs and journals workload transitions
- `gpu-workload-proxy`: execution admission and completion proxy
- `gpu-operator`: one-request local operator backend
- `gpu-setup`: guided setup, discovery and catalog commit

The Debian package adds the GNOME Shell extension and setup application. The
workflow also publishes a checksum file covering all archives and the CycloneDX
source SBOM. Build provenance is recorded in GitHub attestations and can be
checked with `gh attestation verify` on a downloaded asset.

1. Merge through a green PR, and wait for the `main` workflow on the merge commit.
   SonarQube quality gate must pass there.
2. Confirm the planned version in `release-version.json`, then dispatch `release`
   from `main`. There is no version input; the dispatch snapshot supplies the version.
3. The workflow confirms that `main` still points to the dispatch SHA, retests it,
   freezes `release/X.Y.Z`, builds and attests assets, and creates the immutable `vX.Y.Z`
   tag and release through the release GitHub App. It deletes the release branch
   after successful publication. Re-run the failed job from the original run to
   retry the same SHA, even if `main` has advanced. If publication already
   completed, the workflow verifies the immutable tag, assets, and checksum,
   then removes the prepared branch. For an incomplete or mutable publication,
   follow the recovery procedure below.
4. After verified publication, the workflow opens or reuses a next-patch bump PR
   for `release-version.json`. It enables squash auto-merge only after validating
   that the complete PR diff is exactly the expected version line. Required PR
   checks and repository protections still apply. Re-run the failed follow-up job
   to recover a failed bump without publishing another release. A newer planned
   version on main is preserved; a lower version or unrelated PR changes fail closed.

A non-empty `docs/releases/vX.Y.Z.md` becomes the GitHub release notes. The
file is optional; without one the release uses auto-generated notes. `v0.1.6`
and `v0.1.7` shipped without notes files.

The `release-automation` environment, `RELEASE_APP_ID` repository variable, App
installation with Contents write and Pull requests write, release-branch/tag
creation and release-branch deletion bypasses, and GitHub release immutability must be configured
before dispatch. In Settings > General > Releases, enable release immutability.
Set `RELEASE_SETTINGS_REVIEW` on that environment to the five reviewed release
ruleset revisions. Its `immutable_releases: true` value is an owner attestation,
not a live read of the repository setting. Confirm that setting before dispatch;
only future releases become immutable when it is enabled. The workflow validates
the attestation and checks the accessible live rulesets before preparing a release.
When GitHub redacts
bypass actors, the matching reviewed ruleset revision supplies that assurance.
Administration permission is not required. After publication, the workflow
verifies that the release is immutable before cleanup or the next-patch bump. Failed runs do not rewrite existing
tags or releases.
`release-version.json` tracks the next planned release.
For a minor or major release, change it through a normal PR before dispatch.
Go module versions remain Git tags; the version file is release planning metadata.

## Recover a mutable or incomplete publication

The existing `v0.1.0` release was published mutable and is retained with its
original tag and assets. Later version reservations do not certify `v0.1.0`
or bypass the immutable publication check. Read `release-version.json` for the
current planned version.

1. Preserve the existing release, tag, and assets. Release tags prohibit updates
   and deletion; the release App has no bypass for those protections.
2. Confirm release immutability is enabled and the reviewed ruleset revisions
   remain current. Fix the failing validation before dispatching again.
3. Reserve an unused higher version in `release-version.json` through a normal PR
   if the planned tag is already occupied by an invalid publication.
4. Wait for green main validation, then dispatch a new release from `main`.
   Retrying the old run still targets its original version and commit.
5. Check that the new publication is immutable and complete, and that the
   next-patch PR merges after its required checks and reserves the following patch
   version automatically.

Binary archives include full dependency licenses and notices in
`THIRD_PARTY_NOTICES/`, collected for both Linux architectures by pinned
`google/go-licenses/v2` v2.0.1 using the shipped commands' dependency graphs.
GoReleaser regenerates the bundle; snapshot and release gates inspect UUID's BSD
notice and representative transitive notices. Keep collector updates separate from
runtime dependencies, review warnings about non-Go code when dependencies change,
and preserve this directory when redistributing binaries. The SBOM supplements
these full notices; it does not replace them.
