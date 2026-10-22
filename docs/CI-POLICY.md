# CI policy changes

CI dependency lockfiles are data, not approval lists. The trusted base guard validates
the two CI npm lockfiles structurally: npm lockfile v3, registry.npmjs.org HTTPS
tarballs, canonical SHA-512 integrity, no links or bundled packages, and no new
install-script flags relative to the immutable PR base. The root lockfile metadata
must match its manifest. CI manifest changes require independent owner admission.
Root, `.github`, and CI-prefix `.npmrc` files and CI-prefix shrinkwrap files are
forbidden, including when the validated lockfile is unchanged. They must not
redirect npm resolution or shadow the validated package lock.
Registry origin and integrity do not establish package trust: vulnerability audits,
dependency review, and behavioral tests remain required. Installs retain
`--ignore-scripts`. Never add lockfile content hashes to policy.

The guard reads PR contents as data and executes only its trusted base checkout.
Existing executable hash checks and semantic workflow, scanner, and release checks
remain active. The guard workflow, its complete local import closure, and CODEOWNERS
must match the immutable base byte-for-byte or receive independent admission;
being the repository owner or a trusted human author is not an exemption.
An unknown executable digest can be admitted in the same PR when
all of these conditions hold:

- The PR author is a GitHub bot identity, separate from `mickey-kras`.
- The immutable base has `/.github/ @mickey-kras` as its sole CODEOWNERS rule.
- The latest decisive review from owner account ID `24433677` approves the exact
  current head. A dismissal or request for changes invalidates that admission.
- An active, applicable native ruleset requires code-owner review, stale-review
  dismissal, and approval of the latest push by another actor. An administrator
  must configure and verify an empty bypass list before relying on review admission.
- The head and base have not changed while the guard reads its evidence.

Admission does not bypass semantic invariants, dependency audits, tests, or branch
protection. API failures fail closed. Comments and reviews of older commits are
not approvals. If changing an admitted executable, replace its old digest in its
existing policy assertion in the same PR; do not append historical versions.

## Administrator rollout

Provision a dedicated implementation GitHub App or bot account and connect the
implementer to it. Do not share the owner's credential, use the release App, or
grant the implementer administration or bypass privileges. Owner-authored PRs
cannot use review admission; changing commit author text is not identity separation.

Import `.github/rulesets/require-policy-review.json` as an **additional** active
repository ruleset. Keep existing required-check, branch, and tag rulesets. The
rule requires one approving review for every PR targeting main or release branches
and a code-owner review for `.github/**`; verify this repository-wide review change
before activation. Confirm its effective rules and empty bypass list through the
GitHub rules API using administrator access. GitHub omits `bypass_actors` from
ruleset responses to read-only callers. The guard reads all pages of effective
branch rules and rejects any visible nonempty or malformed bypass list; an absent
list is not evidence that no bypass exists. The owner's administrator readback
is a deployment prerequisite and must be repeated after ruleset changes. Record
the ruleset ID and verification in the rollout PR. Do not give the guard an
administrator credential to expose this field. A checked-in JSON file does not
activate GitHub protection.

The initial rollout is a one-time owner-reviewed policy migration: review the
exact bootstrap head and any failing checks before using a specifically authorized
owner bypass. Never disable guards or fabricate a success status. Record any
exception and the exact commit in the PR. Review-based admission remains unavailable
until CODEOWNERS is on the base and native protection is active.

For later changes, the bot opens the PR, checks run, and the owner approves its
exact head. Re-run the trusted policy job after approval. Pushing or updating the
branch requires another review. Native protection prevents a previously green
guard from authorizing a merge after review dismissal or a new push. No review
event checks out or runs contributor code. Complete all required checks before
squash merging. A semantic-policy change still needs the trusted base guard's
invariants satisfied or an explicitly reviewed policy migration.

The implementation PR must be authored by a GitHub App installation identity with REST `user.type` equal to `Bot`. A personal machine account using a PAT remains a `User` and cannot use review admission. Verify this before rollout.
