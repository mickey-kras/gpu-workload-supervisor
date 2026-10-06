# Security policy

[Documentation](docs/README.md) | [Repository](README.md)

Report vulnerabilities through a private GitHub security advisory for this
repository.

The module implements a local control-state engine. A lease fence combines a store incarnation and epoch;
runtime adapters must reject stale fences before admitting work. After a host or
runtime restart, deployment startup ordering must keep execution unavailable until
reconciliation succeeds; this is not automatic proxy startup enforcement. Ordinary
proxy restarts and CLI database opens preserve admission and fences. Backup
restoration requires the explicit incarnation-rotation procedure before startup.
See [startup and recovery](docs/OPERATIONS.md) and [restoration](docs/RESTORING.md).
A transition must record its intent before causing a runtime side effect.

Do not log credentials, workload payloads, or untrusted runtime output.
Host service permissions and access to the SQLite state file are deployment
controls and must be reviewed for each runtime integration. The service UID and
its private state directory are trusted: processes running as that UID can replace
state between validation and SQLite opening it. File checks do not isolate
mutually untrusted processes sharing a UID; use separate service identities.

CI uses pinned GitHub Actions, Go vulnerability analysis, dependency review,
Gitleaks, Semgrep, Trivy, CodeQL, Aislop, and main-only SonarQube. Dependabot
ecosystems, update rules, and auto-merge requirements are in
[REPOSITORY-CONTROLS.md](docs/REPOSITORY-CONTROLS.md#dependabot).

## Dependency licenses

Dependency review blocks high and critical vulnerabilities in runtime, development,
and unknown scopes. License compatibility is reviewed manually; CI does not certify it.

- Separately executed CI tools (including SonarSource's scanner) do not need a
  license exception for each version. Review actual changes to their license or usage.
- Before adding or changing dependencies shipped in a binary, package, or container,
  review the applicable terms, linking/bundling, notices, and source/relinking duties.
- GPL, AGPL, LGPL, SSPL, custom, and unknown terms require context, not an automatic ban.
  Record the decision and required notices in the PR; update third-party notices as needed.
- Existing dependencies are not proof of approval. Review any unrecorded shipped
  dependency before relying on its licensing. Disable auto-merge when manual review is needed.

The repository's own license does not replace dependency licenses.
