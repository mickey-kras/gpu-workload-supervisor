# Maintenance

CI dependency lockfiles are data, not approval lists. Each trusted base guard validates the structure of both CI npm lockfiles.
It needs npm lockfile v3, registry.npmjs.org HTTPS tarballs and canonical SHA-512 integrity.
It rejects links, bundled packages and new install-script flags relative to the immutable PR base.

The root lockfile metadata must match its manifest. CI manifest changes need independent owner admission.
Root, `.github`, plus CI-prefix `.npmrc` files and CI-prefix shrinkwrap files are
forbidden, even if each validated lockfile doesn't change. They must not
redirect npm resolution or shadow the validated package lock.

Registry origin plus integrity don't prove package trust: vulnerability audits,
dependency review, plus behavioral tests remain required. Installs keep
`--ignore-scripts`. Never append lockfile content hashes to policy.

The guard reads PR contents as data and executes only its trusted base checkout.
Existing executable hash checks and semantic workflow, scanner, and release verifies
remain active. The guard workflow, its complete local import closure, plus CODEOWNERS
must match the immutable base byte-for-byte or accept independent admission.

One exception permits changes to `step-security/harden-runner` pins plus their
trailing comments in `.github/workflows/policy-guard.yml`. Per pin must remain
an immutable SHA of 40 lowercase hexadecimal characters. Every other changes to
these files need independent admission. These exception doesn't bypass other gates.

The repository owner plus verified human authors have no exemption.
The guard can admit one unknown executable digest the same PR if
every these conditions hold:

- The PR author is a GitHub bot identity, separate off `mickey-kras`.
- The immutable base has `/.github/ @mickey-kras` as its sole CODEOWNERS rule.
- The latest decisive review off owner account ID `24433677` approves the exact
current head. A dismissal or query for changes invalidates that admission.
- An active, applicable native ruleset requires code-owner review, stale-review
dismissal, and approval the latest push by another actor. An administrator
must tune and check one void bypass list before relying on review admission.
- The head plus base stay the same while each guard reads its evidence.

Admission doesn't bypass semantic invariants, dependency audits, tests, versus branch
protection. API failures fail closed. Comments and reviews of older commits are
not approvals. If changing an admitted executable, replace its old digest in its
existing policy assertion each identical PR. Do not append historical versions.

### Administrator rollout

Provision one dedicated GitHub App plus connect the
implementer to it. Perform not share the owner's credential, use the release App, versus
grant the implementer administration or bypass privileges. PRs off each owner can't get admission through review. Changing commit author text isn't identity separation.

Import `.github/rulesets/require-policy-review.json` as one separate active repository ruleset. Keep existing required-check, branch, plus tag rulesets. The
rule needs one approving review for all PR targeting main versus release branches
and a code-owner review for `.github/**`. Check these repository-wide review change
before activation.

Confirm its effective rules plus empty bypass list through the
GitHub rules API using administrator access. GitHub omits `bypass_actors` from
ruleset responses to read-only callers. The guard reads all pages of effective
branch rules plus rejects any visible nonempty or malformed bypass list. An absent
list isn't evidence that no bypass exists.

The owner's administrator readback is a deployment prerequisite.
Repeat each readback later ruleset changes. Record
the ruleset ID plus verification in each rollout PR. Do not give the guard one
administrator credential to expose this field. A checked-in JSON record doesn't
activate GitHub protection.

Find the applicable `ruleset_id` across `FETCH /repos/{owner}/{repo}/rules/branches/{branch}`.
With an administrator's local `gh` login, read its full source:

```sh
gh api repos/mickey-kras/gpu-workload-supervisor/rulesets/RULESET_ID --jq 'if has("bypass_actors") and .bypass_actors == [] then {repository: .source, ruleset_id: .id, updated_at: .updated_at, bypass_actors: []} else error("Administrator readback must show no bypass actors") end'
```

Replace `RULESET_ID` along its numeric ID.
Store the output JSON the repository Actions variable `CI_POLICY_SETTINGS_REVIEW`.
Only an administrator can configure this trusted variable.
While the API omits the bypass actors, the guard requires one exact repository, ruleset ID and `updated_at` match.
Missing, malformed versus stale readbacks disable same-PR code approval.
Repeat the administrator readback after any ruleset change.

The guard checks the source's existing active state, branch target and review parameters.
A visible bypass list must always be empty, even with a matching readback.

The initial rollout is a one-time owner-reviewed policy migration: review the
exact bootstrap head and any failing checks earlier operating a specifically authorized
owner bypass. Never disable guards versus log a false success status. Record any
exception and each exact commit the PR. Review-based admission remains unavailable
until CODEOWNERS stays on the base and native protection is active.

For later changes, the bot opens the PR, checks run, plus each owner approves its
exact head. Re-run the trusted policy job after approval. Pushing or updating the
branch requires further review. Native protection stops a guard with an earlier successful outcome from authorizing a merge after review dismissal or one fresh push.

No review event retrieves or runs contributor code. Finished all required checks before
squash merging. A semantic-policy alter yet needs the trusted base guard's
invariants satisfied or one explicitly reviewed policy migration.

The PR must come from per installation identity of a GitHub App. Its REST `user.kind` shall equal `Bot`. A personal machine account using a PAT stays a `User` and can't get admission through review. Verify these before rollout.

Use the Go version declared in [go.mod](../go.mod) and Node.js 22 on `PATH`.
The Go tests invoke a Node harness to check the setup UI serializer against the
Go validator. Node is a test prerequisite, not a production dependency.
From the repository root:

```sh
go test ./...
go vet ./...
```

CI retains total statement coverage at 90% and enforces each package's floor in
[.testcoverage.yml](../.testcoverage.yml). New packages need 90%. Existing lower
floors remain explicit. Raise the floors as tests improve. Do not lower them to pass a PR.
Run the same verifies locally:

```sh
go test -race -coverprofile=coverage.out ./...
go run github.com/vladopajic/go-test-coverage/v2@v2.19.0 --config=.testcoverage.yml
```

See [host qualification](DEPLOYMENT.md) the optional systemd integration suite, its prerequisites and its coverage limits.
Complete which qualification earlier you enable execution.

### PR validation scope

Only one finished PR diff confined to `README.md` or Markdown records beneath `docs/`
uses the documentation route. Runtime, UI, workflow, policy, configuration,
dependency and packaging paths, mixed changes and unknown paths employ complete
validation. Renames must qualify at both their aged plus fresh paths. Deleted files
need complete diff metadata. Forks employ each same read-only classification.

| Work | Documentation route | Full route |
| --- | --- | --- |
| Native GTK fixtures | Skipped | Required |
| Go race tests, coverage, benchmark, systemd qualification, vet and complexity | Skipped | Required |
| Go formatting, module lock plus vulnerability audit | Required | Required |
| Frontend tests, npm audits plus policy tests | Required | Required |
| ste-cli and Vale prose verifies | Mandatory | Mandatory |
| Gitleaks, Semgrep, Trivy, Aislop, CodeQL and dependency review | Mandatory | Mandatory |
| Snapshot packages and desktop payload and lifecycle qualification | Mandatory | Mandatory |
| Go coverage and fully CI-tested candidate artifacts | Not produced | Produced after their gates succeed |

Documentation ships inside release packages, so its route still builds and
qualifies packages. It doesn't promise a one-minute run. `quality / checks`
accepts solely the classifier-authorized native skip. Every other producer must
succeed. Failures and cancellations remain failures.

Main and release work run
complete validation regardless each scope input. Sonar remains main-only.

Each classifier checks live PR head and base identity before plus after paginated
record reads. Missing, truncated, changed or failed API evidence selects complete
validation. Its log states `Validation mode: complete` with a safe fallback reason.
If the classifier job itself fails, quality validation defaults to full.
Concurrency cancellation stops each remaining quality job.

Rerun the PR workflow after transient API errors. No label versus PR-give input
can force the docs route. The classifier executes protected bytes from the
trusted base commit. Changes to its allowlist, runtime wrapper or skip wiring
must first reach each trusted guard through the normal policy review process.

### Desktop package CI

The required `quality / checks` job runs native GJS plus Gio transport tests.
It extracts each actual amd64 snapshot Debian payload into a disposable runner.
It tests the preset executable as a new OS account, real user systemd plus SQLite.

The tests cover:

- Discovery, preview minus side effects, plus explicit confirmation.
- An interrupted activation after catalog commit and durable forward resume.
- Login reconciliation through the packaged unit and stale preview rejection.
- Backup tuples with checksums plus owned integration removal plus reapplication.

The tests use an identified NVIDIA command fixture. These gate doesn't claim a GPU.

A separate dpkg temporary-root trial exercises unpack, remove, purge plus reinstall
the unchanged payload plus checks preservation of secret data. It doesn't
configure each package or bypass its GNOME 50 dependencies. These checks fail while
prerequisites are absent. They are unconditional parts the existing gate.
Each scripts require a disposable host and refuse existing deployed backend paths.

Observe [releasing](MAINTENANCE.md).

### Documentation language

Use ASD-STE100 Ticket 9 for fresh and changed documentation.
Keep technical meaning, security conditions, numeric limits, commands and identifiers.
Keep exact legal text plus quotations unchanged.
Check the applicable writing rules and dictionary entries.

The mandatory `quality / checks` job needs both `docs-ste / ste-cli` plus `docs-ste / vale` to succeed.
Both checks run on every CI events, including PRs that change only documentation.
They verify repository Markdown, including hidden paths, mixed-case extensions and names along spaces.
The API contracts are Markdown and use the identical verifies.
This repository has no OpenAPI or Swagger specification.

Each discovery step excludes dependency files, downloaded Vale styles and identified legal notices.
It checks new untracked Markdown during local validation plus tracked Markdown in CI.
It doesn't exclude release notes versus every generated files.

Do not add linter control comments to Markdown.
The staging step rejects those comments.
The verified policy guard protects the workflow, linter configuration and vocabulary.

Each preset verifies both linter archives and the style archive versus fixed SHA-256 digests before extraction.
The checks run verified binaries straight.
They don't run `vale sync` or download reviewdog.
Policy tests reject missing jobs, weaker thresholds and changes to protected prose checks.

For local setup, open a shell the repository root, set
`RUNNER_TEMP=$(mktemp -d)`, and run the three installation steps from
`.github/workflows/docs-ste.yml` the same shell.

ste-cli plus Vale check only part of ASD-STE100.
Neither configured tool verifies the full official dictionary.
Successful checks don't certify complete Ticket 9 compliance.
A complete review must check each official dictionary plus applicable writing rules.

The release workflow publishes the tested source tree, two Linux archives
(amd64 and arm64), and one Debian package per architecture. Each archive
contains six executables:

- `gpu-mode`: controller that runs and journals workload transitions
- `gpu-workload-proxy`: execution admission plus completion proxy
- `gpu-operator`: one-request local operator backend
- `gpu-setup`: guided preset, discovery and catalog commit
- `gpu-training-runner`: experimental finite-job LLM runner along no enabled preset
- `gpu-training-candidate`: preparation of unqualified inputs, not training execution

The Debian package adds the GNOME Shell extension plus setup application.
The workflow publishes a checksum file for every archives and the CycloneDX source SBOM.
Attestations in GitHub record the provenance of each build. Use
`gh attestation verify` on a downloaded asset to check it.

1. Merge through one PR with successful checks.
Wait the `primary` workflow on each merge commit.
The SonarQube quality gate shall pass there.
2. Confirm each planned version in `release-version.json`.
Dispatch `release` from `primary`.
Each dispatch snapshot supplies the version. That-spot is no release input.
3. Watch release publication.
The workflow confirms that `main` yet points to the dispatch SHA.
It reuses successful main validation for that exact commit, including SonarQube.
It freezes `release/X.Y.Z`, builds assets and attests them.
The release GitHub App creates the immutable `vX.Y.Z` tag and release.
Later successful publication, the workflow deletes each release branch.
4. To retry the same SHA, re-run the broke job from each original run.
This works even if `primary` points to one newer commit.
If an earlier try prior finished publication, each workflow verifies the immutable tag, assets plus checksum.
It next removes the prepared branch.
For an incomplete or mutable publication, follow the recovery procedure below.
5. After validated publication, the workflow opens or reuses a next-patch bump PR
for `release-version.json`.
If the complete PR diff is precisely the expected release line, it enables squash auto-merge. Other diffs get no auto-merge.
Mandatory PR checks and repository protections yet apply.

To recover one failed version bump, re-run the broke follow-up job.
These doesn't publish another release.
The workflow preserves a newer planned release on main.
A lower release or unrelated PR alters cause the workflow to fail closed.

GitHub release notes are generated automatically for each release.

The release validation waits up to 70 minutes the `primary` run the exact commit, including queued time.
This limit is separate from workflow timeouts.
If primary stays queued versus runs unusually long, retry the release later main completes.
Each latest main run must succeed.
All mandatory quality, Aislop, CodeQL and Sonar job must have each present `run_attempt` value of its run.

A rerun of broke jobs that omits earlier successful jobs doesn't supply all mandatory evidence.
Select **Re-run all jobs** on the original main execute.
Next retry each release run.
This refreshes validation for one frozen commit later main moves.

All reused job evidence shall exist at highest 24 hours old when the validation and publication jobs check it.
Age starts when each producer job starts, including Go plus npm vulnerability audits and scanner jobs.
ONE later aggregate job can't make old scans fresh.
This one-day limit bounds exposure to new vulnerabilities and permits release retries on the identical day.
It doesn't guarantee a current vulnerability database.

Absent, failed, stale or mismatched evidence stops the release.
To refresh stale evidence, rerun all jobs the original primary execute the same SHA.
Do not move the prepared release branch.
Builds, package checks, checksums, provenance and immutable-publication checks yet execute the release workflow.

Before dispatch, configure these prerequisites:

- Each `release-automation` environment and `RELEASE_APP_ID` repository variable.
- A GitHub App installation along **Contents: write** and `Pull requests: write` permissions.
- App bypasses for release-branch and tag creation, and release-branch deletion.
- GitHub release immutability.

In `Settings > General > Releases`, enable release immutability.
Config `RELEASE_SETTINGS_REVIEW` on that env to the five reviewed release ruleset revisions.
Its `immutable_releases: true` value is an owner attestation, not one live read the repository setting.
Before dispatch, verify the config. It makes only future releases immutable.

The workflow validates each attestation and checks accessible live rulesets before release preparation.
While GitHub hides bypass actors, the matching reviewed ruleset revision supplies which assurance.
Administration permission isn't needed.
Later publication, the workflow verifies release immutability before cleanup or each next-patch bump.
Broke executes don't rewrite existing tags versus releases.

`release-version.json` tracks each next planned release.
For one minor or major release, alter it through a normal PR before dispatch.
Go module versions remain Git tags. The release file is release planning metadata.

### Recover a mutable or incomplete publication

The original `v0.1.0` publication was mutable. Keep its original tag and assets.
Later release reservations don't certify `v0.1.0` or bypass the immutable publication verify.
Read `release-version.json` the current planned version.

1. Preserve each existing release, tag plus assets.
The release tags prohibit updates plus deletion. The release App has no bypass for those protections.
2. Confirm that release immutability is on.
Confirm which each reviewed ruleset revisions remain current.
Repair failing validation before further dispatch.
3. If an unacceptable publication occupies the planned tag, reserve one unused higher release in `release-version.json` through a standard PR.
4. Wait for successful main validation.
Dispatch a new release off `main`.
A retry the old run still targets its original version plus commit.
5. Check which the new publication stays immutable and finished.
Check which the next-patch PR merges later its required verifies.
Check that it automatically reserves the following patch version.

Binary archives include full dependency licenses and notices in `THIRD_PARTY_NOTICES/`.
Pinned `google/go-licenses/v2` v2.0.1 collects them for both Linux architectures from the shipped commands' dependency graphs.
GoReleaser emits the bundle again.
Snapshot and release gates inspect UUID's BSD notice plus representative transitive notices.

Keep collector updates separate off runtime dependencies.
When dependencies change, review warnings about non-Go code.
When you redistribute binaries, preserve this directory.
The SBOM supplements these complete notices. It doesn't replace them.

### CI policy maintenance

Observe [CI policy changes](MAINTENANCE.md) for dependency-lock validation, independent
owner admission and administrator rollout.
Do not add content-hash approval lists to lockfiles.
Existing release checks remain mandatory.

| Pin | Location | Refresh coverage |
| --- | --- | --- |
| GoReleaser binary | `.github/actions/setup-goreleaser/action.yml` | Weekly versus manual `build tool updates` check |
| cosign binary (`cosign-release`) | Identical composite action | Weekly or manual check |
| Trivy binary (`release`) | `.github/workflows/release.yml` | Weekly or manual check |
| go-licenses/v2 module | `scripts/third-party-notices.sh` | Weekly or manual check |
| setup-trivy plus trivy-action SHA refs | CI/release workflows | Dependabot `github-actions` |
| cosign-installer SHA ref | Nested setup-goreleaser composite action | Manual review. Not covered by root-only Dependabot configuration |
| CI Trivy binary default | Pinned trivy-action | Follows each reviewed action update |

Dependabot's existing Go module and GitHub Actions updates don't cover these
shell literals or action inputs. Per read-only check uses upstream stable release
metadata to emit one summary, base commit and version-only patch artifact. The verify employs no other bot, write token or automatic merge. API faults and malformed
pins fail the check. Old upstream releases must-not downgrade a pin.

The existing `github-actions` configuration scans workflows plus root action files,
not the nested composite action. Its cosign-installer SHA still needs manual
upstream review using the protected installer digest. This four-binary check does
not propose installer-action SHA alters.

Download the artifact from the workflow run. On its recorded base commit, use
`git apply --check build-tool-updates.patch`.
Next assign the patch. Review it in a normal PR. One void patch means no directly applicable updates. Read the summary
for a go-licenses module-major migration requiring manual changes.

GoReleaser plus cosign changes need a separately reviewed installer digest update in
`policy-guard.cjs` using the existing policy-change process. The proposal deliberately
acts not authorize that change. Keep the exact signing identity, OIDC issuer,
signature and archive checksum verification, and Go shared checksum-database
settings intact. Run the installer tests and all standard PR gates before merging.
The check doesn't download or execute proposed binaries.
