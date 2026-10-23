# CI policy changes

CI dependency lockfiles are data, not approval lists. The trusted base guard validates the structure of both CI npm lockfiles.
It requires npm lockfile v3, registry.npmjs.org HTTPS tarballs and canonical SHA-512 integrity.
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
remain active. The guard workflow, its complete local import closure, and CODEOWNERS
must match the immutable base byte-for-byte or receive independent admission.
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

## Administrator rollout

Provision a dedicated implementation GitHub App and connect the
implementer to it. Do not share the owner's credential, use the release App, or
grant the implementer administration or bypass privileges. PRs from the owner cannot get admission through review. Changing commit author text is not identity separation.

Import `.github/rulesets/require-policy-review.json` as an separate active repository ruleset. Keep existing required-check, branch, and tag rulesets. The
rule requires one approving review for every PR targeting main or release branches
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
branch requires another review. Native protection prevents a guard with an earlier successful result from authorizing a merge after review dismissal or a new push.

No review event retrieves or runs contributor code. Complete all required checks before
squash merging. A semantic-policy change still needs the trusted base guard's
invariants satisfied or an explicitly reviewed policy migration.

The implementation PR must come from the installation identity of a GitHub App. Its REST `user.type` must equal `Bot`. A personal machine account using a PAT remains a `User` and cannot get admission through review. Check this before rollout.
