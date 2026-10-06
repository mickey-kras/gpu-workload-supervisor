# Repository workflow

[Documentation](../docs/README.md) | [Repository](../README.md)

- `main` is the only permanent branch. Use squash merges through PRs.
- Work branches match `^(feat|fix|refactor|docs|ci|security)/[a-z0-9]+(-[a-z0-9]+)*$`.
- Dependabot branches are accepted only for PRs authored by `dependabot[bot]`.
- Eight native rulesets protect `main`, restrict branch and tag creation, protect release branches, and make `v*` tags immutable. The `branch-policy` check validates the full branch regex on same-repository PRs.
- Required checks are `quality / checks`, `aislop / aislop status`, `codeql / analyze`, `branch-policy / branch name`, `dependency-review / dependency review`, and `guard`. The last check comes from the trusted base workflow.
- Same-repository PR branches are updated after main pushes when the release App is configured. Dependabot rebases its own branches.
- Releases follow [RELEASING.md](../docs/RELEASING.md): manual dispatch from the validated `main` tip at the planned version, immutable `vX.Y.Z` tag, attested assets, and an automatic next-patch bump PR.
- CI checks the GoReleaser configuration and builds snapshot archives before publication.

See [repository controls](../docs/REPOSITORY-CONTROLS.md) for the active settings.

