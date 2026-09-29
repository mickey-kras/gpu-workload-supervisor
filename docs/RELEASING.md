# Source and binary releases

The release workflow publishes the tested source tree and two Linux archives,
each containing the `gpu-mode` and `gpu-workload-proxy` executables for amd64 or
arm64. It also publishes a checksum file covering all archives and the CycloneDX
source SBOM. Build provenance is recorded in GitHub attestations and can be
checked with `gh attestation verify` on a downloaded asset.

1. Merge through a green PR, and wait for the `main` workflow on the merge commit.
   SonarQube quality gate must pass there.
2. Dispatch `release` from `main` with a new plain SemVer version such as `0.1.0`.
3. The workflow confirms that `main` still points to the dispatch SHA, retests it,
   freezes `release/X.Y.Z`, builds and attests assets, and creates the immutable `vX.Y.Z`
   tag and release through the release GitHub App. It deletes the release branch
   after successful publication. Re-run the failed job from the original run to
   retry the same SHA, even if `main` has advanced. If publication already
   completed, the workflow verifies the immutable tag, assets, and checksum,
   then removes the prepared branch. An incomplete release needs manual recovery.

The `release-automation` environment, `RELEASE_APP_ID` repository variable, App
installation with Contents write and Administration read, release-branch/tag
creation and deletion bypasses, and GitHub release immutability must be configured
before dispatch. Set `RELEASE_SETTINGS_REVIEW` on that environment to the five
reviewed release ruleset revisions. The workflow checks the live rulesets and
immutability before preparing a release. Failed runs do not rewrite existing
tags or releases.
There is no version-bump PR because
the Go module version is the Git tag; no in-tree version constant exists.
