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
See [startup and recovery](docs/OPERATIONS.md#use-and-recover-workloads) and [restoration](docs/OPERATIONS.md#make-a-backup-restore-and-roll-back-state).
A transition must record its intent before it causes a runtime side effect.

Do not log credentials, workload payloads, or untrusted runtime output.
The execution proxy defaults to trusted local callers of its own user. It closes
connections from other users. Opt-in adapter ingress
separates gateway execution credentials from trusted terminal-adapter completion
credentials and hides registration tokens from client response metadata. It does
not replace:

- Remote requester authentication, CSRF and policy
- Host identity isolation
- Direct runtime bypass prevention

Trusted backends must not echo credentials or registration tokens in response
bodies or logs. See
[adapter ingress](docs/INTEGRATION.md#trusted-adapter-ingress).

Host service permissions and access to the SQLite state file are deployment
controls. Review them for each runtime integration.
The deployment trusts the service UID and its private state directory.
Processes with that UID can replace state between validation and the SQLite open operation.
File checks do not isolate mutually untrusted processes that share a UID.
Use separate service identities.

CI uses pinned GitHub Actions, Go vulnerability analysis, dependency review,
Gitleaks, Semgrep, Trivy, CodeQL, Aislop, and main-only SonarQube. Dependabot
ecosystems and update rules are in [dependabot.yml](.github/dependabot.yml).
Auto-merge requirements are in the [workflow](.github/workflows/dependabot-auto-merge.yml)
and its [eligibility checks](.github/scripts/dependabot-auto-merge.cjs).

## Agents and tool executors

Do not trust an agent, its tools or the documents that it reads. The supervisor alone controls workload lifecycle, configuration and model files. Text from a model, a tool or a document cannot start, stop or change a workload.

| Part | Identity | Access |
| --- | --- | --- |
| Supervisor, proxy and runtimes | Service UID | State, control and model files |
| Agent and tool executor | A different, unprivileged UID | Inference through the proxy only |
| Model updater | Operator, outside the runtime units | Model files, then setup and model checks |

The boundary uses Unix identities, not a network sandbox:

- The supervisor has no control socket. Lifecycle commands open the SQLite state. Only the service UID can open the state: it must be a private `0600` file in directories that other users cannot write.
- In `trusted` ingress, the proxy closes connections from other UIDs. An agent of a different UID needs [adapter ingress](docs/INTEGRATION.md#trusted-adapter-ingress) and the execution credential. Do not give it the completion credential.
- The proxy forwards only the configured routes. A native vLLM binding accepts only `POST` to `/v1/chat/completions`, `/v1/completions` and `/v1/embeddings`, and `GET /v1/models`.
- The vLLM key file is private to the service UID. Setup refuses `VLLM_SERVER_DEV_MODE`, plugins and adapters that load at run time. Thus vLLM does not open its development routes, and a caller cannot load an adapter.
- Supervisor-managed vLLM units mount the model files read-only.

Run each agent and tool executor as a different UID. Do not add that UID to the service UID group. Do not mount the state directory, the key files, the user systemd directory or the model directory into its container. Do not run tools as the service UID: file checks do not isolate processes that share a UID.

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
