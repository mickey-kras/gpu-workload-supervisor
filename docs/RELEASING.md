# Source and binary releases

[Documentation](README.md) | [Repository](../README.md)

The release workflow publishes the tested source tree, two Linux archives
(amd64 and arm64), and one Debian package per architecture. Each archive
contains six executables:

- `gpu-mode`: controller that runs and journals workload transitions
- `gpu-workload-proxy`: execution admission and completion proxy
- `gpu-operator`: one-request local operator backend
- `gpu-setup`: guided setup, discovery and catalog commit
- `gpu-training-runner`: experimental finite-job LLM runner with no enabled preset
- `gpu-training-candidate`: preparation of unqualified inputs, not training execution

The Debian package adds the GNOME Shell extension and setup application.
The workflow publishes a checksum file for all archives and the CycloneDX source SBOM.
Attestations in GitHub record the provenance of each build.
Use `gh attestation verify` on a downloaded asset to check its provenance.

1. Merge through a PR with successful checks.
   Wait for the `main` workflow on the merge commit.
   The SonarQube quality gate must pass there.
2. Confirm the planned version in `release-version.json`.
   Dispatch `release` from `main`.
   The dispatch snapshot supplies the version. There is no version input.
3. Watch release publication.
   The workflow confirms that `main` still points to the dispatch SHA.
   It reuses successful main validation for that exact commit, including SonarQube.
   It freezes `release/X.Y.Z`, builds assets and attests them.
   The release GitHub App creates the immutable `vX.Y.Z` tag and release.
   After successful publication, the workflow deletes the release branch.
4. If publication fails, re-run the failed job from the original run.
   Even if `main` points to a newer commit, this retries the same SHA.
   If publication is complete, the workflow verifies the immutable tag, assets and checksum.
   It then removes the prepared branch.
   For an incomplete or mutable publication, follow the recovery procedure below.
5. Check the next-patch bump PR for `release-version.json` after verified publication.
   The workflow opens or reuses this PR.
   It enables squash auto-merge only if the complete PR diff is exactly the expected version line.
   Required PR checks and repository protections still apply.

To recover a failed version bump, re-run the failed follow-up job.
This does not publish another release.
The workflow preserves a newer planned version on main.
A lower version or unrelated PR changes cause the workflow to fail closed.

A non-empty `docs/releases/vX.Y.Z.md` supplies the GitHub release notes.
Without this optional file, the release uses automatically generated notes.
`v0.1.6` and `v0.1.7` shipped without notes files.

The release validation waits up to 70 minutes for the `main` run for the exact commit, including queued time.
This limit is separate from workflow timeouts.
If main stays queued or runs unusually long, retry the release after main completes.
The latest main run must succeed.
Every required quality, Aislop, CodeQL and Sonar job must belong to its current run execution.

A rerun of failed jobs that omits earlier successful jobs does not supply all required evidence.
Select **Re-run all jobs** on the original main run.
Then retry the release run.
This also refreshes validation for a frozen commit after main moves.

All reused job evidence must be at most 24 hours old when the validation and publication jobs check it.
Age starts when each producer job starts, including Go and npm vulnerability audits and scanner jobs.
A later aggregate job cannot make old scans fresh.
This one-day limit bounds exposure to new vulnerabilities and permits release retries on the same day.
It does not guarantee a current vulnerability database.

Missing, failed, stale or mismatched evidence stops the release.
To refresh stale evidence, rerun all jobs of the original main run for the same SHA.
Do not move the prepared release branch.
Builds, package checks, checksums, provenance and immutable-publication checks still run in the release workflow.

Before dispatch, configure these prerequisites:

- The `release-automation` environment and `RELEASE_APP_ID` repository variable.
- A GitHub App installation with **Contents: write** and **Pull requests: write** permissions.
- App bypasses for release-branch and tag creation, and release-branch deletion.
- GitHub release immutability.

In **Settings > General > Releases**, enable release immutability.
Set `RELEASE_SETTINGS_REVIEW` on that environment to the five reviewed release ruleset revisions.
Its `immutable_releases: true` value is an owner attestation, not a live read of the repository setting.
Before dispatch, confirm the setting. It makes only future releases immutable.

The workflow validates the attestation and checks accessible live rulesets before release preparation.
When GitHub hides bypass actors, the matching reviewed ruleset revision supplies that assurance.
Administration permission is not necessary.
After publication, the workflow verifies release immutability before cleanup or the next-patch bump.
Failed runs do not rewrite existing tags or releases.

`release-version.json` tracks the next planned release.
For a minor or major release, change it through a normal PR before dispatch.
Go module versions remain Git tags. The version file is release planning metadata.

## Recover a mutable or incomplete publication

The original `v0.1.0` publication was mutable. Keep its original tag and assets.
Later version reservations do not certify `v0.1.0` or bypass the immutable publication check.
Read `release-version.json` for the current planned version.

1. Preserve the existing release, tag and assets.
   The release tags prohibit updates and deletion. The release App has no bypass for those protections.
2. Confirm that release immutability is on.
   Confirm that the reviewed ruleset revisions are current.
   Repair failing validation before another dispatch.
3. If an invalid publication occupies the planned tag, reserve an unused higher version in `release-version.json` through a normal PR.
4. Wait for successful main validation.
   Dispatch a new release from `main`.
   A retry of the old run still targets its original version and commit.
5. Check that the new publication is immutable and complete.
   Check that the next-patch PR merges after its required checks.
   Check that it automatically reserves the following patch version.

Binary archives include full dependency licenses and notices in `THIRD_PARTY_NOTICES/`.
Pinned `google/go-licenses/v2` v2.0.1 collects them for both Linux architectures from the shipped commands' dependency graphs.
GoReleaser generates the bundle again.
Snapshot and release gates inspect UUID's BSD notice and representative transitive notices.

Keep collector updates separate from runtime dependencies.
When dependencies change, review warnings about non-Go code.
When you redistribute binaries, preserve this directory.
The SBOM supplements these full notices. It does not replace them.

## CI policy maintenance

See [CI policy changes](CI-POLICY.md) for dependency-lock validation, independent
owner admission and administrator rollout.
Do not add content-hash approval lists to lockfiles.
Existing release checks remain mandatory.
