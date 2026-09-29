# Repository workflow

- `main` is the only permanent branch. Use squash merges through PRs.
- Work branches match `^(feat|fix|refactor|docs|ci|security)/[a-z0-9]+(-[a-z0-9]+)*$`.
- Dependabot branches are accepted only for PRs authored by `dependabot[bot]`.
- Eight native rulesets protect `main`, restrict branch and tag creation, protect release branches, and make `v*` tags immutable. The `branch-policy` check validates the full branch regex on same-repository PRs.
- Required checks are `quality / checks`, `aislop / aislop status`, `codeql / analyze`, `branch-policy / branch name`, `dependency-review / dependency review`, and `guard`. The last check comes from the trusted base workflow.
- Same-repository PR branches are updated after main pushes when the release App is configured. Dependabot rebases its own branches.
- Releases are manually dispatched from the current `main` tip with a plain SemVer version. The workflow verifies a successful main push run, retests the commit, freezes a `release/X.Y.Z` branch, and publishes an attested source module archive under an immutable `vX.Y.Z` tag.
- The project has no executable or image yet. Introduce binary or container publishing only with the runtime entry point and corresponding build and smoke tests.

See [onboarding](../docs/ONBOARDING.md) for the settings that activate these files.
