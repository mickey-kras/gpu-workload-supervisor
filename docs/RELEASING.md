# Source module releases

This repository currently provides Go packages, without an executable. The release
workflow publishes an archive of the tested source tree, a checksum file, a
CycloneDX SBOM, and build provenance to a GitHub Release. It does not publish an
empty container image.

1. Merge through a green PR, and wait for the `main` workflow on the merge commit.
   SonarQube quality gate must pass there.
2. Dispatch `release` from `main` with a new plain SemVer version such as `0.1.0`.
3. The workflow confirms that `main` still points to the dispatch SHA, retests it,
   freezes `release/X.Y.Z`, attests assets, and creates the immutable `vX.Y.Z`
   tag and release through the release GitHub App. It deletes the release branch
   after successful publication. A failed run retains it for retry at the same SHA.

The `release-automation` environment, `RELEASE_APP_ID` repository variable, App
installation and release-branch/tag creation and deletion bypasses must be
configured before dispatch. Failed runs do not rewrite existing tags or releases.
There is no version-bump PR because
the Go module version is the Git tag; no in-tree version constant exists.
