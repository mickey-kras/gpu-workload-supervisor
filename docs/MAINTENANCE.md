# Maintenance

## CI policy changes

CI dependency lockfiles are data, not approval lists. The trusted base guard validates the structure of both CI npm lockfiles.
It needs npm lockfile v3, registry.npmjs.org HTTPS tarballs and canonical SHA-512 integrity.
It rejects links, bundled packages and new install-script flags relative to the immutable PR base.

The root lockfile metadata must match its manifest. CI manifest changes need independent owner admission.
Root, `.github`, and CI-prefix `.npmrc` files and CI-prefix shrinkwrap files are
forbidden, even if the validated lockfile does not change. They must not
redirect npm resolution or shadow the validated package lock.

Registry origin and integrity do not prove package trust: vulnerability audits,
dependency review, and behavioral tests remain required. Installs keep
`--ignore-scripts`. Never add lockfile content hashes to policy.

The guard reads PR contents as data and executes only its trusted base checkout.
Existing executable hash checks and semantic workflow, scanner, and release checks
stay active. The guard workflow, its complete local import closure, and CODEOWNERS
must match the immutable base byte-for-byte or receive independent admission.

One exception permits changes to `step-security/harden-runner` pins and their
trailing comments in `.github/workflows/policy-guard.yml`. Each pin must remain
an immutable SHA of 40 lowercase hexadecimal characters. All other changes to
these files need independent admission. This exception does not bypass other gates.

The repository owner and trusted human authors have no exemption.
The guard can admit an unknown executable digest in the same PR if
all these conditions hold:

- The PR author is a GitHub bot identity, separate from `mickey-kras`.
- The immutable base has `/.github/ @mickey-kras` as its sole CODEOWNERS rule.
- The latest decisive review from owner account ID `24433677` approves the exact
  current head. A dismissal or request for changes invalidates that admission.
- An active, applicable native ruleset requires code-owner review, stale-review
  dismissal, and approval of the latest push by another actor. An administrator
  must configure and check an empty bypass list before relying on review admission.
- The head and base stay the same while the guard reads its evidence.

Admission does not bypass semantic invariants, dependency audits, tests, or branch
protection. API failures fail closed. Comments and reviews of older commits are
not approvals. If changing an admitted executable, replace its old digest in its
existing policy assertion in the same PR. Do not append historical versions.

### Administrator rollout

Provision a dedicated implementation GitHub App and connect the
implementer to it. Do not share the owner's credential, use the release App, or
grant the implementer administration or bypass privileges. PRs from the owner cannot get admission through review. Changing commit author text is not identity separation.

Import `.github/rulesets/require-policy-review.json` as a separate active repository ruleset. Keep existing required-check, branch, and tag rulesets. The
rule needs one approving review for every PR targeting main or release branches
and a code-owner review for `.github/**`. Check this repository-wide review change
before activation.

Confirm its effective rules and empty bypass list through the
GitHub rules API using administrator access. GitHub omits `bypass_actors` from
ruleset responses to read-only callers. The guard reads all pages of effective
branch rules and rejects any visible nonempty or malformed bypass list. An absent
list is not evidence that no bypass exists.

The owner's administrator readback is a deployment prerequisite.
Repeat the readback after ruleset changes. Record
the ruleset ID and verification in the rollout PR. Do not give the guard an
administrator credential to expose this field. A checked-in JSON file does not
activate GitHub protection.

Find the applicable `ruleset_id` through `GET /repos/{owner}/{repo}/rules/branches/{branch}`.
With an administrator's local `gh` login, read its full source:

```sh
gh api repos/mickey-kras/gpu-workload-supervisor/rulesets/RULESET_ID --jq 'if has("bypass_actors") and .bypass_actors == [] then {repository: .source, ruleset_id: .id, updated_at: .updated_at, bypass_actors: []} else error("Administrator readback must show no bypass actors") end'
```

Replace `RULESET_ID` with its numeric ID.
Store the output JSON in the repository Actions variable `CI_POLICY_SETTINGS_REVIEW`.
Only an administrator can configure this trusted variable.
When the API omits the bypass actors, the guard needs an exact repository, ruleset ID and `updated_at` match.
Missing, malformed or stale readbacks disable same-PR code approval.
Repeat the administrator readback after any ruleset change.

The guard checks the source's current active state, branch target and review parameters.
A visible bypass list must always be empty, even with a matching readback.

The initial rollout is a one-time owner-reviewed policy migration: review the
exact bootstrap head and any failing checks before using a specifically authorized
owner bypass. Never disable guards or report a false success status. Record any
exception and the exact commit in the PR. Review-based admission remains unavailable
until CODEOWNERS is on the base and native protection is active.

For later changes, the bot opens the PR, checks run, and the owner approves its
exact head. Re-run the trusted policy job after approval. Pushing or updating the
branch requires another review. Native protection stops a guard with an earlier successful result from authorizing a merge after review dismissal or a new push.

No review event retrieves or runs contributor code. Complete all required checks before
squash merging. A semantic-policy change still needs the trusted base guard's
invariants satisfied or an explicitly reviewed policy migration.

The implementation PR must come from the installation identity of a GitHub App. Its REST `user.type` must equal `Bot`. A personal machine account using a PAT stays a `User` and cannot get admission through review. Check this before rollout.

## Development

Use the Go version declared in [go.mod](../go.mod) and Node.js 22 on `PATH`.
The Go tests invoke a Node harness to check the setup UI serializer against the
Go validator. Node is a test prerequisite, not a production dependency.
From the repository root:

```sh
go test ./...
go vet ./...
```

CI keeps total statement coverage at 90% and enforces each package's floor in
[.testcoverage.yml](../.testcoverage.yml). New packages need 90%. Existing lower
floors are explicit. Raise the floors as tests improve. Do not lower them to pass a PR.
Run the same checks locally:

```sh
go test -race -coverprofile=coverage.out ./...
go run github.com/vladopajic/go-test-coverage/v2@v2.19.0 --config=.testcoverage.yml
```

See [host qualification](DEPLOYMENT.md#qualify-the-host) for the optional systemd integration suite, its prerequisites and its coverage limits.
Complete that qualification before you enable execution.

### PR validation scope

Only a complete PR diff confined to `README.md` or Markdown files below `docs/`
uses the documentation route. Runtime, UI, workflow, policy, configuration,
dependency and packaging paths, mixed changes and unknown paths use full
validation. Renames must qualify at both their old and new paths. Deleted files
need complete diff metadata. Forks use the same read-only classification.

| Work | Documentation route | Full route |
| --- | --- | --- |
| Native GTK fixtures | Skipped | Required |
| Go race tests, coverage, benchmark, systemd qualification, vet and complexity | Skipped | Required |
| Go formatting, module lock and vulnerability audit | Required | Required |
| Frontend tests, npm audits and policy tests | Required | Required |
| ste-cli and Vale prose checks | Required | Required |
| Gitleaks, Semgrep, Trivy, Aislop, CodeQL and dependency review | Required | Required |
| Snapshot packages and desktop payload and lifecycle qualification | Required | Required |
| Go coverage and fully CI-tested candidate artifacts | Not produced | Produced after their gates succeed |

Documentation ships inside release packages, so its route still builds and
qualifies packages. It does not promise a one-minute run. `quality / checks`
accepts only the classifier-authorized native skip. Every other producer must
succeed. Failures and cancellations remain failures.

Main and release work run
full validation regardless of the scope input. Sonar remains main-only.

The classifier checks live PR head and base identity before and after paginated
file reads. Missing, truncated, changed or failed API evidence selects full
validation. Its log states `Validation mode: full` with a safe fallback reason.
If the classifier job itself fails, quality validation also defaults to full.
Concurrency cancellation stops the remaining quality work.

Rerun the PR workflow after transient API errors. No label or PR-provided input
can force the documentation route. The classifier runs protected bytes from the
trusted base commit. Changes to its allowlist, runtime wrapper or skip wiring
must first reach the trusted guard through the normal policy review process.

### Desktop package CI

The required `quality / checks` job runs native GJS and Gio transport tests.
It extracts the actual amd64 snapshot Debian payload into a disposable runner.
It tests the setup executable as a new OS account, real user systemd and SQLite.

The tests cover:

- Discovery, preview without side effects, and explicit confirmation.
- An interrupted activation after catalog commit and durable forward resume.
- Login reconciliation through the packaged unit and stale preview rejection.
- Backup tuples with checksums and owned integration removal and reapplication.

The tests use an identified NVIDIA command fixture. This gate does not claim a GPU.

A separate dpkg temporary-root test exercises unpack, remove, purge and reinstall
of the unchanged payload and checks preservation of private data. It does not
configure the package or bypass its GNOME 50 dependencies. These checks fail when
prerequisites are absent. They are unconditional parts of the existing gate.
The scripts need a disposable host and refuse existing installed backend paths.

See [releasing](MAINTENANCE.md#source-and-binary-releases).

### Documentation language

Use ASD-STE100 Issue 9 for new and changed documentation.
Keep technical meaning, security conditions, numeric limits, commands and identifiers.
Keep exact legal text and quotations unchanged.
Check the applicable writing rules and dictionary entries.

The required `quality / checks` job needs both `docs-ste / ste-cli` and `docs-ste / vale` to succeed.
Both checks run on all CI events, including PRs that change only documentation.
They check repository Markdown, including hidden paths, mixed-case extensions and names with spaces.
The API contracts are Markdown and use the same checks.
This repository has no OpenAPI or Swagger specification.

The discovery step excludes dependency files, downloaded Vale styles and identified legal notices.
It checks new untracked Markdown during local validation and tracked Markdown in CI.
It does not exclude release notes or all generated files.

Do not add linter control comments to Markdown.
The staging step rejects those comments.
The trusted policy guard protects the workflow, linter configuration and vocabulary.

The setup checks both linter archives and the style archive against fixed SHA-256 digests before extraction.
The checks run verified binaries directly.
They do not run `vale sync` or download reviewdog.
Policy tests reject missing jobs, weaker thresholds and changes to protected prose checks.

For local setup, open a shell in the repository root, set
`RUNNER_TEMP=$(mktemp -d)`, and run the three installation steps from
`.github/workflows/docs-ste.yml` in the same shell.

ste-cli and Vale check only part of ASD-STE100.
Neither configured tool checks the full official dictionary.
Successful checks do not certify full Issue 9 compliance.
A complete review must check the official dictionary and applicable writing rules.

## Source and binary releases

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
Attestations in GitHub record the provenance of each build. Use
`gh attestation verify` on a downloaded asset to check it.

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
4. To retry the same SHA, re-run the failed job from the original run.
   This works even if `main` points to a newer commit.
   If an earlier try already finished publication, the workflow verifies the immutable tag, assets and checksum.
   It then removes the prepared branch.
   For an incomplete or mutable publication, follow the recovery procedure below.
5. After verified publication, the workflow opens or reuses a next-patch bump PR
   for `release-version.json`.
   If the complete PR diff is exactly the expected version line, it enables squash auto-merge. Other diffs get no auto-merge.
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
Every required quality, Aislop, CodeQL and Sonar job must have the current `run_attempt` value of its run.

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
- A GitHub App installation with **Contents: write** and `Pull requests: write` permissions.
- App bypasses for release-branch and tag creation, and release-branch deletion.
- GitHub release immutability.

In `Settings > General > Releases`, enable release immutability.
Set `RELEASE_SETTINGS_REVIEW` on that environment to the five reviewed release ruleset revisions.
Its `immutable_releases: true` value is an owner attestation, not a live read of the repository setting.
Before dispatch, check the setting. It makes only future releases immutable.

The workflow validates the attestation and checks accessible live rulesets before release preparation.
When GitHub hides bypass actors, the matching reviewed ruleset revision supplies that assurance.
Administration permission is not necessary.
After publication, the workflow verifies release immutability before cleanup or the next-patch bump.
Failed runs do not rewrite existing tags or releases.

`release-version.json` tracks the next planned release.
For a minor or major release, change it through a normal PR before dispatch.
Go module versions remain Git tags. The version file is release planning metadata.

### Recover a mutable or incomplete publication

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
GoReleaser produces the bundle again.
Snapshot and release gates inspect UUID's BSD notice and representative transitive notices.

Keep collector updates separate from runtime dependencies.
When dependencies change, review warnings about non-Go code.
When you redistribute binaries, preserve this directory.
The SBOM supplements these full notices. It does not replace them.

### CI policy maintenance

See [CI policy changes](MAINTENANCE.md#ci-policy-changes) for dependency-lock validation, independent
owner admission and administrator rollout.
Do not add content-hash approval lists to lockfiles.
Existing release checks remain mandatory.

## Build tool updates

| Pin | Location | Update coverage |
| --- | --- | --- |
| GoReleaser binary | `.github/actions/setup-goreleaser/action.yml` | Weekly or manual `build tool updates` check |
| cosign binary (`cosign-release`) | Same composite action | Weekly or manual check |
| Trivy binary (`version`) | `.github/workflows/release.yml` | Weekly or manual check |
| go-licenses/v2 module | `scripts/third-party-notices.sh` | Weekly or manual check |
| setup-trivy and trivy-action SHA refs | CI/release workflows | Dependabot `github-actions` |
| cosign-installer SHA ref | Nested setup-goreleaser composite action | Manual review. Not covered by root-only Dependabot configuration |
| CI Trivy binary default | Pinned trivy-action | Follows the reviewed action update |

Dependabot's existing Go module and GitHub Actions updates do not cover these
shell literals or action inputs. The read-only check uses upstream stable release
metadata to produce a summary, base commit and version-only patch artifact. The check uses no other bot, write token or automatic merge. API errors and malformed
pins fail the check. Old upstream releases cannot downgrade a pin.

The existing `github-actions` configuration scans workflows and root action files,
not the nested composite action. Its cosign-installer SHA still needs manual
upstream review with the protected installer digest. This four-binary check does
not propose installer-action SHA changes.

Download the artifact from the workflow run. On its recorded base commit, use
`git apply --check build-tool-updates.patch`.
Then apply the patch. Review it in a normal PR. An empty patch means no directly applicable updates. Read the summary
for a go-licenses module-major migration requiring manual changes.

GoReleaser and cosign changes need a separately reviewed installer digest update in
`policy-guard.cjs` using the existing policy-change process. The proposal deliberately
does not authorize that change. Keep the exact signing identity, OIDC issuer,
signature and archive checksum verification, and Go public checksum-database
settings intact. Run the installer tests and all normal PR gates before merging.
The check does not download or execute proposed binaries.
