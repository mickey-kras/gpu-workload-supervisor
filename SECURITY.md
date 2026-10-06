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
Gitleaks, Semgrep, Trivy, CodeQL, and main-only SonarQube. Dependabot
auto-merge requires verified bot commits, patch or minor updates, a known
compatibility score of at least 75% for every dependency, and passing branch
checks. Major or unscored updates need manual review.

