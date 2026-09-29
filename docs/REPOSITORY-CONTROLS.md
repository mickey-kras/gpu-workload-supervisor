# Repository controls

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

The release App needs Contents write, Pull requests write for PR branch updates,
and Administration read for release ruleset and immutability checks. The
`RELEASE_APP_ID` repository variable is the App client ID used by the token
action. The `RELEASE_APP_PRIVATE_KEY` secret, the `release-automation`
environment, and repository release immutability are required for publication.

The environment variable `RELEASE_SETTINGS_REVIEW` records the owner-reviewed
numeric App actor ID and the current revisions of five release rulesets. Its
format is `{ "app_id": 4956768, "immutable_releases": true, "rulesets":
{ "RULESET_ID": "UPDATED_AT" } }`. If any release ruleset changes, review its
bypass actor, scope, and protection, then replace the revision map with the
current values. A release fails closed when the reviewed value is absent or
stale, a live rule weakens, or immutable releases are disabled.

See [branching](../.github/BRANCHING.md), [Dependabot](DEPENDABOT.md), and
[releasing](RELEASING.md) for operating details.
