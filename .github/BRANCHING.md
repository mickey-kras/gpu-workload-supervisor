# Repository workflow

[Documentation](../docs/README.md) | [Repository](../README.md)

- `main` is the only permanent branch. Use squash merges through PRs.
- Work branches match `^(feat|fix|refactor|docs|ci|security)/[a-z0-9]+(-[a-z0-9]+)*$`.
- Dependabot branches are accepted only for PRs authored by `dependabot[bot]`.
- Eight native rulesets protect `main`, restrict branch and tag creation, protect release branches, and make `v*` tags immutable. The `branch-policy` check validates the full branch regex on same-repository PRs.
- Required checks are `quality / checks`, `aislop / aislop status`, `codeql / analyze`, `branch-policy / branch name`, `dependency-review / dependency review`, and `guard`. The last check comes from the trusted base workflow.
- Same-repository PR branches are updated after main pushes when the release App is configured. Dependabot rebases its own branches.
- Releases are manually dispatched from the current `main` tip using the planned plain SemVer version from `release-version.json`. The workflow verifies successful main validation, retests the commit, freezes a `release/X.Y.Z` branch, and publishes attested source and Linux amd64/arm64 binary archives under an immutable `vX.Y.Z` tag.
- CI checks the GoReleaser configuration and builds snapshot archives before publication.

See [repository controls](../docs/REPOSITORY-CONTROLS.md) for the active settings.

After publication, a one-line next-patch version bump PR is queued for squash auto-merge after required checks pass.

