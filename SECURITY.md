# Security policy

Report vulnerabilities through a private GitHub security advisory for this
repository.

The current module implements a local control-state engine. It has no network
API or executable yet. A lease fence combines a store incarnation and epoch;
runtime adapters must reject stale fences before admitting work. After
restart, admission stays closed until reconciliation observes the actual
workload. A transition must record its intent before causing a runtime
side effect.

Do not log credentials, workload payloads, or untrusted runtime output.
Host service permissions and access to the SQLite state file are deployment
controls and must be reviewed when the executable is introduced.

CI uses pinned GitHub Actions, Go vulnerability analysis, dependency review,
Gitleaks, Semgrep, Trivy, CodeQL, Aislop, and main-only SonarQube. Dependabot
auto-merge requires verified bot commits, patch or minor updates, a known
compatibility score of at least 75% for every dependency, and passing branch
checks. Major or unscored updates need manual review.
