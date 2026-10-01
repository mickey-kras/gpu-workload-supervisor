# Dependabot

Go modules, the pinned Aislop and policy npm toolchains, and GitHub Actions are updated daily.
Dependabot owns its branch rebases. `go.sum` and the npm lockfile must be updated
within the same PR.
CodeQL subactions are grouped so initialization and analysis always update together.
Grouped updates retain the per-dependency compatibility and update-type checks below.

The trusted `pull_request_target` automation checks the exact Dependabot identity,
verified bot commits, same-repository origin, a minor or patch update, and a known
compatibility score of at least 75% for every changed dependency. Eligible PRs
queue squash auto-merge; required branch checks still decide when the merge happens.
Major updates and updates without a score remain for manual review.
The compatibility lookup and merge command use the workflow token. Enable
repository auto-merge and squash merges before relying on this automation.
The release App and `RELEASE_APP_ID` are needed for PR branch updates and releases,
not for Dependabot compatibility lookup.

The 30-minute refresh rechecks open PRs and dispatches missing validation after a
Dependabot merge. It never runs code from a PR head with write permissions.

Review the pinned Semgrep image digest, Trivy CLI version, and the pinned
`dependabot/fetch-metadata` checkout when updating scanner tooling. These are
not dependency manifests and are not covered by the scheduled ecosystems.
