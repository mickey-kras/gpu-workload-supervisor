# Repository settings

Apply these settings after this PR is merged and the new PR checks have appeared.
The files under `.github/rulesets/` are importable ruleset definitions. They do
not become active by being committed.

1. Repository merge settings: enable squash only and auto-merge; disable merge
   commits and rebase merges; enable automatic deletion of merged branches.
2. Enable the dependency graph now so `dependency-review` can pass on this PR.
   Enable Dependabot alerts, code scanning, code quality, and secret scanning as
   available. Run `main` once after merge to register CodeQL, Trivy,
   Aislop, and SonarQube analyses.
3. Import all eight rulesets under `.github/rulesets/`. Confirm the required check
   names and GitHub Actions integration against the actual PR checks before
   activating `Protect default branch`. Keep zero approving reviews for a
   single-maintainer repo; require resolution of review threads. The three
   release branch rulesets use the existing release App as their creation
   and deletion bypass actor.
4. Confirm that the SonarQube project key `gpu-workload-supervisor` exists with
   the intended quality profile and gate. Its main-only workflow connects as
   `tag:github-sonar` and uses the existing Tailscale and Sonar secrets.
5. Set the repository variable `RELEASE_APP_ID` for the GitHub App whose private
   key is stored as `RELEASE_APP_PRIVATE_KEY`. Install that App on this repo with
   Contents and Pull requests write plus Administration read permissions. Create the
   `release-automation` environment. Confirm the App actor in the tag-creation
   ruleset before importing it.
6. Enable release immutability in repository Settings > General > Releases. The
   release workflow verifies this setting before publishing. It affects future
   releases only.
7. Verify the `main` run and SonarQube, then check the PR updater and a
   Dependabot PR before relying on auto-merge. A source release can be dispatched
   after the `main` run succeeds.

The initial state-engine tests cover 53.4% of statements. CI enforces a 53%
floor to prevent a regression while the controller tests are being added. The
router's 90% floor should be adopted after coverage reaches it.

The release process freezes the tested `main` commit in `release/X.Y.Z`,
publishes an immutable release from that commit, and deletes the branch after
success. Re-run a failed job from the original workflow run to retry the same
commit. A complete release is verified before a cleanup retry; an incomplete
publication requires manual recovery.
