# Security policy

[Documentation](docs/README.md) | [Repository](README.md)

Report vulnerabilities through a private GitHub security advisory for this
repository.

The module implements a local control-state engine.
A lease fence combines a store incarnation and epoch.
Runtime adapters must reject stale fences before they admit work. After a host or
runtime restart, deployment startup ordering must keep execution unavailable until
reconciliation succeeds. This does not enforce proxy startup automatically.

Ordinary proxy restarts and CLI database opens preserve admission and fences. Backup
restoration requires the explicit incarnation-rotation procedure before startup.
See [startup and recovery](docs/OPERATIONS.md) and [restoration](docs/RESTORING.md).
A transition must record its intent before it causes a runtime side effect.

Do not log credentials, workload payloads, or untrusted runtime output.
The execution proxy defaults to trusted local callers. Opt-in adapter ingress
separates gateway execution credentials from trusted terminal-adapter completion
credentials and hides registration tokens from client response metadata. It does
not replace remote requester authentication, CSRF and policy, host identity isolation,
or direct runtime bypass prevention. Trusted backends must not echo credentials or
registration tokens in response bodies or logs. See
[adapter ingress](docs/EXECUTION-PROXY.md#trusted-adapter-ingress).

Host service permissions and access to the SQLite state file are deployment controls.
Review these controls for each runtime integration.
The deployment trusts the service UID and its private state directory.
Processes with that UID can replace state between validation and the SQLite open operation.
File checks do not isolate mutually untrusted processes that share a UID.
Use separate service identities.

CI uses pinned GitHub Actions, Go vulnerability analysis, dependency review,
Gitleaks, Semgrep, Trivy, CodeQL, Aislop, and main-only SonarQube. Dependabot
ecosystems and update rules are in [dependabot.yml](.github/dependabot.yml).
Auto-merge requirements are in the [workflow](.github/workflows/dependabot-auto-merge.yml)
and its [eligibility checks](.github/scripts/dependabot-auto-merge.cjs).

## Dependency licenses

Dependency review blocks high and critical vulnerabilities in runtime, development,
and unknown scopes. Review license compatibility manually. CI does not certify it.

- Separately executed CI tools (including SonarSource's scanner) do not need a
  license exception for each version. Review actual changes to their license or usage.
- Before adding or changing dependencies shipped in a binary, package, or container,
  review the applicable terms, linking and bundling, notices, and source and relinking duties.
- GPL, AGPL, LGPL, SSPL, custom, and unknown terms need context, not an automatic ban.
  Record the decision and required notices in the PR. Update third-party notices as needed.
- Existing dependencies are not proof of approval. Review any unrecorded shipped
  dependency before relying on its licensing. Disable auto-merge when manual review is necessary.

The repository's own license does not replace dependency licenses.
