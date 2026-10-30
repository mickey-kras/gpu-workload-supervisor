# Maintenance

- [Development](#development)
- [Releases](#source-and-binary-releases)
- [Publication recovery](#recover-a-mutable-or-incomplete-publication)
- [CI policy](#ci-policy-changes)
- [Tool updates](#build-tool-updates)

## Development

Use the [go.mod](../go.mod) Go version and Node.js 22 on `PATH`.
Go tests invoke Node to compare setup UI serialization with the Go validator. Production does not need Node.

Run from the repository root:

```sh
go test ./...
go vet ./...
```

Total statement coverage must reach 90%. [.testcoverage.yml](../.testcoverage.yml) sets package floors.
New packages need 90%. Raise the explicit lower floors as coverage improves. Never lower them to pass.

```sh
go test -race -coverprofile=coverage.out ./...
go run github.com/vladopajic/go-test-coverage/v2@v2.19.0 --config=.testcoverage.yml
```

Complete [host qualification](DEPLOYMENT.md#qualify-the-host) before enabling execution. That section gives optional systemd-suite prerequisites and limits.

### PR validation scope

Only complete diffs confined to `README.md` or Markdown under `docs/` select documentation validation.
Mixed, unknown, runtime, UI, workflow, policy, configuration, dependency and packaging paths need full validation.

Renames must qualify at both paths. Deletions need complete metadata. Forks use the same read-only classification.

| Work | Documentation | Full |
| --- | --- | --- |
| Native GTK fixtures. Go race, coverage, benchmark, systemd, vet and complexity | Skip | Run |
| Go formatting, module lock and vulnerability audit. Frontend, npm and policy tests | Run | Run |
| STE and Vale. Gitleaks, Semgrep, Trivy, Aislop, CodeQL and dependency review | Run | Run |
| Snapshot packages, desktop payload and lifecycle qualification | Run | Run |
| Go coverage and fully CI-tested candidate artifacts | None | Produce after successful gates |

Documentation ships in packages, so this route still builds and qualifies them. It promises no one-minute run.
`quality / checks` permits only the classifier-authorized native skip. All other producers must succeed. Failure or cancellation stays failure.

Main and release always use full validation. Sonar runs only on main.

The protected base classifier verifies live head and base before and after paginated reads.
Missing, truncated, changed or failed evidence, including classifier failure, defaults to full validation.

Logs report `Validation mode: full` and a safe reason. Concurrency cancellation stops remaining quality work.
Rerun after transient API errors. Labels or PR inputs cannot force documentation mode.

Allowlist, wrapper and skip-wiring changes need normal trusted-base policy review.

### Desktop package CI

`quality / checks` tests native GJS and Gio transport and the actual amd64 snapshot Debian payload with a fresh OS account, real user-systemd and SQLite.
It covers discovery, side-effect-free preview and confirmation, catalog-commit interruption and forward resume.

It also checks packaged login reconciliation, stale previews, checksummed backup tuples, and owned integration removal and reapplication.
The identified NVIDIA command fixture provides no GPU qualification.

Separate dpkg temporary-root tests unpack, remove, purge and reinstall unchanged payload and check private-data preservation. They neither configure packages nor bypass GNOME 50 dependencies.
Both gates are unconditional, fail without prerequisites, need disposable hosts and reject existing installed backend paths.

### Documentation language

Use ASD-STE100 Issue 9 writing rules and dictionary. Preserve technical meaning, security conditions, numbers, commands, identifiers, exact quotations and legal text.
`quality / checks` requires both `docs-ste / ste-cli` and `docs-ste / vale` on every CI event, including documentation-only PRs.

Discovery covers repository Markdown: hidden paths, mixed-case extensions, spaces, API contracts, release notes and generated files.
It includes local untracked files and CI tracked files, but excludes dependencies, downloaded styles and identified legal notices.

No OpenAPI or Swagger specification exists.
Staging rejects linter-control comments. The trusted guard protects workflow, configuration and vocabulary.

Installation checks both binary archives and the style archive against fixed SHA-256 digests, then runs verified binaries directly: no `vale sync` or reviewdog download.
Policy tests reject missing jobs, weaker thresholds and changes to protected checks.

Locally, set `RUNNER_TEMP=$(mktemp -d)` at the repository root. Run the three installation steps from `.github/workflows/docs-ste.yml` in that shell.
Neither tool checks the complete official dictionary. Passing gates does not certify Issue 9 compliance. Review applicable rules and dictionary entries separately.

## Source and binary releases

The workflow publishes tested source, amd64 and arm64 Linux archives, Debian packages for both architectures, archive checksums and a CycloneDX source SBOM.
Archives contain six executables:

| Executable | Purpose |
| --- | --- |
| `gpu-mode` | Runs and journals transitions |
| `gpu-workload-proxy` | Execution admission and completion |
| `gpu-operator` | One-request local backend |
| `gpu-setup` | Setup, discovery, catalog commit |
| `gpu-training-runner` | Experimental finite-job LLM runner. No enabled preset |
| `gpu-training-candidate` | Unqualified input preparation. No training |

Debian packages add the GNOME extension and setup application. Check the downloaded build provenance with `gh attestation verify`.

1. Merge a checked PR. Wait for successful exact-commit main validation, including SonarQube.
2. Check `release-version.json`. Dispatch `release` from main. The dispatch snapshot supplies the version. No version input exists.

3. The workflow confirms unchanged main, reuses exact-commit validation, freezes `release/X.Y.Z`, builds and attests assets, and publishes immutable `vX.Y.Z` through the release App.
4. Verified publication deletes the prepared branch and opens or reuses the next-patch version PR. Only the exact expected version-line diff gets squash auto-merge. Normal checks and protections still apply.

| Retry condition | Required action or behavior |
| --- | --- |
| Same SHA, even after main advances | Rerun the original failed job. Completed publication verifies immutable tag, assets and checksum before branch removal |
| Version-bump failure | Rerun follow-up only. No new release. Preserve newer planned versions. A lower version or unrelated PR changes fail closed |
| Incomplete or mutable publication | Follow [recovery](#recover-a-mutable-or-incomplete-publication). Never rewrite tags or releases |
| Main queued or slow | Validation waits 70 minutes including queue time, separately from workflow timeouts. Retry after main completes |
| Missing, failed, stale or mismatched evidence | Rerun **all jobs** of the original exact-SHA main run, then retry release. Never move the prepared branch |

The latest main run must succeed. All required quality, Aislop, CodeQL and Sonar jobs must match its current `run_attempt`. Reruns of failed jobs cannot reuse earlier success.
Each producer's evidence must be at most 24 hours old at validation and publication, measured from its start, including Go and npm audits and scanners.

Later aggregates cannot refresh old scans. This limit permits same-day retries and bounds vulnerability exposure, but guarantees no current vulnerability database.
The release still runs builds, package checks, checksums, provenance and immutable-publication checks.

Use a nonempty `docs/releases/vX.Y.Z.md` for notes. Otherwise GitHub generates them (`v0.1.6` and `v0.1.7` had no files).
`release-version.json` plans the next release, not Go module versions, which remain tags. Reserve minor or major versions through normal PRs.

Before dispatch, configure `release-automation`, repository variable `RELEASE_APP_ID`, and an installed App with **Contents: write** and `Pull requests: write`.
Allow App bypasses for release-branch and tag creation and release-branch deletion.

Enable `Settings > General > Releases` immutability. It affects only future releases.
Set the environment `RELEASE_SETTINGS_REVIEW` to five reviewed release-ruleset revisions.

Its `immutable_releases: true` is owner attestation, not live setting evidence. Check the setting before dispatch.
The workflow validates attestation and live accessible rulesets before preparation. Matching reviewed revisions cover hidden bypass actors without administration permission.

Publication immutability must pass before cleanup or version bump.

### Recover a mutable or incomplete publication

Keep original mutable `v0.1.0` and all existing tags and assets. Later reservations certify nothing about it.
Tag update and deletion protections have no release-App bypass.

1. Preserve the publication. Confirm immutability and current ruleset reviews. Repair failed validation.
2. If an invalid publication occupies the planned tag, reserve an unused higher version through a normal `release-version.json` PR.

3. Wait for successful main validation. Dispatch from main. Old-run retries keep their original version and commit.
4. Check the complete immutable publication, required next-patch PR checks and merge, and reservation of the following patch.

Archives contain full dependency licenses and notices in `THIRD_PARTY_NOTICES/`.
Pinned `google/go-licenses/v2` v2.0.1 collects both architectures' shipped-command graphs. GoReleaser recreates the bundle.

Snapshot and release gates inspect UUID's BSD and representative transitive notices.
Keep collector updates separate from runtime dependencies. Review non-Go-code warnings when dependencies change.

Redistribution must preserve notices. The SBOM supplements, never replaces them.

<a id="ci-policy-maintenance"></a>

## CI policy changes

Lockfiles are data, never hash approval lists. The trusted base guard reads PR data but executes only its base checkout.
Both CI npm locks need v3, registry.npmjs.org HTTPS tarballs, canonical SHA-512, and root metadata matching manifests.

Reject links, bundled packages and new install-script flags relative to immutable base.
CI manifests need independent owner admission.

Root, `.github` and CI-prefix `.npmrc` and CI-prefix shrinkwrap files are forbidden even with unchanged locks: no registry redirects or lock shadowing.
Keep `--ignore-scripts`. Registry and integrity do not replace audits, dependency review or behavioral tests.

Executable hashes and semantic workflow, scanner and release checks remain mandatory.
Guard workflow, complete local import closure and CODEOWNERS must match immutable base or receive independent admission.

Only `step-security/harden-runner` pins and trailing comments in `.github/workflows/policy-guard.yml` have an exception: immutable SHAs of 40 lowercase hex characters, with all other gates retained.
Owners and trusted humans have no exemption.

An unknown executable digest needs all these conditions:

- An author from a GitHub App installation with REST `user.type` equal to `Bot`, separate from `mickey-kras`. PAT machine accounts remain `User`.
- Base's sole CODEOWNERS rule is `/.github/ @mickey-kras`.
- Owner account ID `24433677` gives the latest decisive approval of the exact head. Dismissal or changes-request invalidates it.
- Active applicable native rules need code-owner review, stale-review dismissal and another actor's latest-push approval. An administrator verifies no bypass actors.
- Head and base remain unchanged during evidence reads.

API failures fail closed. Comments or old-head reviews never approve.
Admission bypasses no semantic invariant, audit, test or protection.

Replace the changed executables' old digests in existing assertions within the PR. Do not append historical digests.

### Administrator rollout

Use a dedicated implementation App, never owner credentials or the release App. Grant no administration or bypass privileges.
Owner-authored PRs cannot receive admission through review. Commit-author text changes do not separate identity.

Import `.github/rulesets/require-policy-review.json` as a separate active rule, retaining existing check, branch and tag rules.
It requires one approval for every PR targeting main or release branches, and code-owner review for `.github/**`. Review this repository-wide change before activation.

Checked-in JSON activates nothing.

Use API access as an administrator to confirm effective rules and an empty bypass list. Read-only responses omit `bypass_actors`.
The guard paginates effective branch rules, rejects visible nonempty or malformed lists, and treats absence as unknown.

Do not give it admin credentials.
Find `ruleset_id` through `GET /repos/{owner}/{repo}/rules/branches/{branch}`, then run with an administrator’s local `gh` login:

```sh
gh api repos/mickey-kras/gpu-workload-supervisor/rulesets/RULESET_ID --jq 'if has("bypass_actors") and .bypass_actors == [] then {repository: .source, ruleset_id: .id, updated_at: .updated_at, bypass_actors: []} else error("Administrator readback must show no bypass actors") end'
```

Replace `RULESET_ID` with its numeric ID. Record the ID and verification in the rollout PR.
Store output JSON in administrator-only Actions variable `CI_POLICY_SETTINGS_REVIEW`.

Repeat after every ruleset change: hidden bypass lists need exact repository, ID and `updated_at` matches. Absent, malformed or stale readbacks disable same-PR admission.
The guard also checks current source active state, branch target and review parameters. Visible bypass lists must always be empty.

Bootstrap needs exact-head owner review, inspection of failing checks and specifically authorized one-time bypass.
Never disable guards or falsify success. Record the exception and commit in the PR.

Review admission stays unavailable until CODEOWNERS reaches base and native protection activates.

Later: bot opens PR, checks run, owner approves exact head, trusted policy job reruns.
Any push or branch update needs new review. Native rules invalidate earlier guard success after dismissal or new push.

Review events never retrieve or run contributor code. Finish all checks before squash merge.
Semantic-policy changes must satisfy trusted-base invariants or use an explicitly reviewed migration.

## Build tool updates

| Pin | Location | Update source |
| --- | --- | --- |
| GoReleaser, cosign `cosign-release` | `.github/actions/setup-goreleaser/action.yml` | Weekly or manual `build tool updates` |
| Trivy `version` | `.github/workflows/release.yml` | Same check |
| go-licenses/v2 | `scripts/third-party-notices.sh` | Same check |
| setup-trivy and trivy-action SHAs | CI and release workflows | Dependabot `github-actions` |
| cosign-installer SHA | Nested setup-goreleaser action | Manual upstream review plus protected installer digest |
| CI Trivy default | Pinned trivy-action | Reviewed action update |

Dependabot covers workflows and root actions and Go modules, not these shell literals or inputs or nested installer actions.
The read-only four-binary check uses upstream stable metadata for a summary, base commit and version-only patch. No extra bot, write token, auto-merge, binary execution or download or installer-SHA proposal.

API errors or malformed pins fail. Older releases cannot downgrade pins.

Download the artifact. At its recorded base run `git apply --check build-tool-updates.patch`, apply, then review a normal PR.
Empty patches mean no applicable update. Summaries identify go-licenses major migrations needing manual changes.

GoReleaser and cosign updates need separate reviewed `policy-guard.cjs` installer digests under normal policy review. Proposals do not authorize them.
Preserve signing identity, OIDC issuer, signatures, archive checksums and Go public checksum-database settings.

Run installer tests and all normal PR gates before merge.
