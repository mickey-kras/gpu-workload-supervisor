# Repository controls

[Documentation](README.md) | [Repository](../README.md)

The repository uses squash merges and auto-merge, with automatic deletion of
merged branches. Merge commits and rebase merges are disabled. The eight active
GitHub rulesets are the source of truth; their one-time JSON import files are
not kept in the repository.

`Protect default branch` requires the `quality / checks`, `aislop / aislop status`,
`codeql / analyze`, `branch-policy / branch name`,
`dependency-review / dependency review`, and `guard` checks, plus resolved
review threads, code scanning, and code quality. Work branch names and `v*`
tag names have dedicated creation rules. Release branches have dedicated
creation, protection, and deletion rules; release tags have creation and
immutable protection rules. Only the release App (actor ID `4956768`) bypasses
the three release creation/deletion rules.

The Go quality workflow enforces formatting, a tidy module lock, race-tested
90% statement coverage, vet, vulnerability scanning, scanner toolchain audit,
GoReleaser snapshot validation, Gitleaks, Semgrep, and Trivy. Pull requests also
run Aislop, CodeQL, and dependency review. The `main` workflow runs SonarQube
through Tailscale using the `tag:github-sonar` identity and enforces its quality
gate. Repository Advanced Security settings should keep the dependency graph,
Dependabot alerts, code scanning, code quality, and secret scanning enabled.

The release App needs Contents write and Pull requests write for PR branch
updates and version bumps. The workflow uses accessible ruleset reads and an
owner-reviewed settings attestation; it does not request Administration read. The
`RELEASE_APP_ID` repository variable is the App client ID used by the token
action. The `RELEASE_APP_PRIVATE_KEY` secret, the `release-automation`
environment, and repository release immutability are required for publication.

The environment variable `RELEASE_SETTINGS_REVIEW` records the owner-reviewed
numeric App actor ID and the current revisions of five release rulesets. Its
format is `{ "app_id": 4956768, "immutable_releases": true, "rulesets":
{ "RULESET_ID": "UPDATED_AT" } }`. If any release ruleset changes, review its
bypass actor, scope, and protection, then replace the revision map with the
current values. The immutable-release field attests to the owner-checked setting;
it is not a live setting check. Confirm Settings > General > Releases has release
immutability enabled before dispatch. A release fails closed when the reviewed
value is absent or stale or a live rule weakens. After publication, an actual
immutable release is required before branch cleanup or the automatic version bump.

Work branches match `^(feat|fix|refactor|docs|ci|security)/[a-z0-9]+(-[a-z0-9]+)*$`;
the `branch-policy` check validates the full regex on same-repository PRs.
Dependabot branches are accepted only for PRs authored by `dependabot[bot]`.
Same-repository PR branches are updated after main pushes when the release App
is configured; Dependabot rebases its own branches. Releases follow
[RELEASING.md](RELEASING.md): manual dispatch from the validated `main` tip at
the planned version, immutable `vX.Y.Z` tag, attested assets, and an automatic
next-patch bump PR.

## Dependabot

Scheduled ecosystems and groups live in
[.github/dependabot.yml](../.github/dependabot.yml). `go.sum` and the npm
lockfiles must be updated within the same PR.

The trusted `pull_request_target` automation checks the exact Dependabot
identity, verified bot commits, same-repository origin, a minor or patch update,
and a known compatibility score of at least 75% for every changed dependency.
Eligible PRs queue squash auto-merge; required branch checks still decide when
the merge happens. Major updates and updates without a score remain for manual
review. The compatibility lookup and merge command use the workflow token.
Enable repository auto-merge and squash merges before relying on this
automation. The release App and `RELEASE_APP_ID` are needed for PR branch
updates and releases, not for Dependabot compatibility lookup.

The daily refresh rechecks open PRs and dispatches missing validation after a
Dependabot merge. It never runs code from a PR head with write permissions.

Review the pinned Semgrep image digest, Trivy CLI version, and the pinned
`dependabot/fetch-metadata` checkout when updating scanner tooling. These are
not dependency manifests and are not covered by the scheduled ecosystems.
