# Source and binary releases

The release workflow publishes the tested source tree and two Linux archives,
each containing the `gpu-mode` and `gpu-workload-proxy` executables for amd64 or
arm64. It also publishes a checksum file covering all archives and the CycloneDX
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
   then removes the prepared branch. An incomplete release needs manual recovery.
4. After verified publication, the workflow opens or reuses a next-patch bump PR
   for `release-version.json`. It enables squash auto-merge only after validating
   that the complete PR diff is exactly the expected version line. Required PR
   checks and repository protections still apply. Re-run the failed follow-up job
   to recover a failed bump without publishing another release. A newer planned
   version on main is preserved; a lower version or unrelated PR changes fail closed.

The `release-automation` environment, `RELEASE_APP_ID` repository variable, App
installation with Contents write, Pull requests write, and Administration read, release-branch/tag
creation and deletion bypasses, and GitHub release immutability must be configured
before dispatch. Set `RELEASE_SETTINGS_REVIEW` on that environment to the five
reviewed release ruleset revisions. The workflow checks the live rulesets and
immutability before preparing a release. Failed runs do not rewrite existing
tags or releases.
`release-version.json` starts at `0.1.0` and tracks the next planned release.
For a minor or major release, change it through a normal PR before dispatch.
Go module versions remain Git tags; the version file is release planning metadata.
