# Dependabot

Go modules, the pinned Aislop and policy npm toolchains, and GitHub Actions are updated daily.
Dependabot owns its branch rebases. `go.sum` and the npm lockfile must be updated
within the same PR.

The trusted `pull_request_target` automation checks the exact Dependabot identity,
verified bot commits, same-repository origin, a minor or patch update, and a known
compatibility score of at least 75% for every changed dependency. Eligible PRs
queue squash auto-merge; required branch checks still decide when the merge happens.
Major updates and updates without a score remain for manual review.
The compatibility lookup uses a repository-scoped installation token from the
release App. The merge command uses the workflow token. Keep the App installed
with Contents read access and configure `RELEASE_APP_ID`.

The 30-minute refresh rechecks open PRs and dispatches missing validation after a
Dependabot merge. It never runs code from a PR head with write permissions.
