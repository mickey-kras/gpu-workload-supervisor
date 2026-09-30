# GPU Workload Supervisor

[![PR validation](https://github.com/mickey-kras/gpu-workload-supervisor/actions/workflows/pr-validation.yml/badge.svg?event=pull_request)](https://github.com/mickey-kras/gpu-workload-supervisor/actions/workflows/pr-validation.yml?query=event%3Apull_request)
[![coverage](https://img.shields.io/badge/coverage-%E2%89%A590%25%20%28CI--gated%29-brightgreen)](https://github.com/mickey-kras/gpu-workload-supervisor/blob/main/.github/workflows/ci.yml)
[![codeql](https://img.shields.io/github/check-runs/mickey-kras/gpu-workload-supervisor/main?nameFilter=codeql%20%2F%20analyze&label=codeql&logo=github)](https://github.com/mickey-kras/gpu-workload-supervisor/actions/workflows/main.yml?query=branch%3Amain)
[![aislop](https://badges.scanaislop.com/score/mickey-kras/gpu-workload-supervisor.svg)](https://scanaislop.com/mickey-kras/gpu-workload-supervisor)
[![main + SonarQube](https://github.com/mickey-kras/gpu-workload-supervisor/actions/workflows/main.yml/badge.svg?branch=main)](https://github.com/mickey-kras/gpu-workload-supervisor/actions/workflows/main.yml?query=branch%3Amain)
[![license: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Crash-safe ownership, admission, and switching for mutually exclusive GPU workloads.

## Scope

The supervisor owns:

- workload ownership and desired state
- admission control
- lease fencing
- transition journaling
- recovery state
- execution-route gating

It does not own job orchestration, content policy, workload APIs, or runtime data.

## State model

New stores start closed and require reconciliation:

```text
owner=supervisor
desiredWorkload=idle
activeWorkload=unknown
phase=reconciling
health=healthy
admission=closed
```

A lease fence combines a store-incarnation UUID with a monotonic epoch. A fresh store creates a new incarnation. An explicit restore rotates the incarnation; ordinary restarts preserve it. A transition rotates the epoch before work is drained.

SQLite uses WAL mode, full synchronous writes, foreign keys, and one database connection. Transition side effects are recorded as intent and observation events.

## Development

```sh
go test ./...
go vet ./...
```

## Controller

Runtime identities, health endpoints, release behavior, and resource thresholds are deployment configuration:

```sh
gpu-mode \
  -text-unit TEXT.service \
  -media-unit MEDIA.service \
  -text-health-url http://127.0.0.1:PORT/health \
  -media-health-url http://127.0.0.1:PORT/health \
  -media-release-url http://127.0.0.1:PORT/release \
  -gpu-index 0 \
  -release-max-used-mib LIMIT \
  -nvidia-smi /ABSOLUTE/PATH/nvidia-smi \
  -systemctl /ABSOLUTE/PATH/systemctl \
  status|reconcile|recover|resolve-work|text|media|idle
```

The default `-media-stop-mode unload` releases media models and keeps its UI
running; it requires `-media-release-url` and a separately enforced execution gate.
Use `-media-stop-mode stop-service` to stop the media unit instead. In this mode,
the release URL is optional, text and idle require media to be stopped, and starting
either runtime requires both units stopped and GPU memory at or below the configured
release threshold. Transitional, failed, or concurrent units fail closed; repair
the units explicitly before recovery. The UI is unavailable outside media mode.

This policy provides service lifecycle exclusion, not request fencing. Disable
independent runtime startup and updates in deployment configuration. Direct
runtime requests bypass admission and registered-work draining; execution gates
are still required for supervised requests. Use the same stop policy for every
command sharing a state store.

The state directory must be private and owned by the current user. Commands use an exclusive file lock. Opening the store applies pending migrations. Boot reconciliation does not resume incomplete work. Interrupted transitions and latched errors require explicit recovery.

Ownership changes always require a target. Add the normal runtime flags before the command:

```sh
gpu-mode [runtime flags] -target text take-control
gpu-mode [runtime flags] -target media user-switch
gpu-mode [runtime flags] -target idle return-control
```

- `take-control` changes supervisor ownership to user ownership. It closes admission, rotates the fence, drains registered work, and verifies the target before committing ownership.
- `user-switch` selects `text`, `media`, or `idle` while user-owned. `return-control` selects the supervisor's workload explicitly. Both authorize terminating all current user work, including queued media work and same-target transfers. They stop both runtime units before starting the selected target.
- `text`, `media`, and `idle` remain supervisor-only commands. They reject user-owned state.
- User ownership keeps supervisor admission closed. Healthy, stable user execution bypasses lease registration only for the selected workload. Execution is blocked while switching, idle, or in an error state.

User execution requests hold a shared cross-process handoff lock. User switches and returns close the gate, stop user runtimes, and wait up to `-drain-timeout` for forwarding handlers to exit before restarting anything. A stalled handler makes the operation fail closed; cancel the client request or stop the proxy before recovery. All proxies sharing a state database must use this version's handoff locking before enabling ownership commands.

Failed and interrupted operations retain the last committed owner and require explicit recovery. Stopped user work is never restarted by rollback. For supervisor ownership, use `recover`. For user ownership, inspect the runtime and run `gpu-mode [runtime flags] -target text|media|idle recover-user` with the workload you intend to keep. This command verifies the target without starting or stopping runtimes, preserves user ownership, and keeps supervisor admission closed. An idle recovery verifies GPU memory release. If verification fails, repair or stop runtimes explicitly and retry. `reconcile` never automatically takes control from the user. Every transfer, user switch, and recovery rotates the fence and records durable source/target ownership and transition outcomes.

If admitted work cannot report completion (for example, forwarding failed before either party received its registration token), stop every `gpu-workload-proxy` instance for this state file and wait for shutdown. Run `gpu-mode` with the normal runtime flags plus `-resolve-reason 'operator incident reference' resolve-work`. Proxy instances hold a shared lifetime lock; `resolve-work` refuses to run until they are gone, then closes admission, rotates the fence, stops both runtimes, verifies they are inactive and GPU memory is released, and atomically marks unfinished work abandoned with an audit record. Do not use an older proxy binary without the lifetime lock during this operation. If any stop or verification fails, work remains unfinished and admission stays closed. Run `recover` after successful resolution, then restart proxies and switch workload as needed. This operation terminates all running work, so use it only after investigating the orphaned requests.

Restored databases require an explicit `gpu-mode -state /PRIVATE/PATH/state.db restore-state` before any proxy starts. This command needs no runtime flags. It does not copy a backup; follow the [restore procedure](docs/RESTORING.md) for ordering and validation. Normal restart does not rotate the fence.

## Execution proxy

The proxy forwards non-mutating routes and gates configured execution routes:

```sh
gpu-workload-proxy \
  -state /PRIVATE/PATH/state.db \
  -listen 127.0.0.1:8090 \
  -upstream http://127.0.0.1:PORT \
  -workload media \
  -execute-route POST:/execute \
  -completion-path /_gpu-workload-supervisor/v1/work/finish
```

During supervisor ownership, gated requests require:

- a unique request ID
- the current lease incarnation and epoch
- stable compatible workload state
- healthy state
- open admission

Default headers:

- `X-Request-ID`
- `X-Workload-Lease-Incarnation`
- `X-Workload-Lease-Epoch`
- `X-Workload-Registration-Token` (generated by the proxy for admitted execution)

The proxy removes caller-supplied lease control headers before forwarding. For an admitted execution route, it then forwards the validated, normalized request ID and lease fence in the configured headers, along with a fresh proxy-generated `X-Workload-Registration-Token`. These values are added after hop-by-hop header filtering, so the upstream receives the registered tuple even if the caller nominated those headers in `Connection`. For safe routes, passthrough routes, and user-owned execution, control headers remain stripped and no work is registered.

Work is registered before forwarding and remains active after submission. The proxy also returns the registration token to the caller in `X-Workload-Registration-Token`. Either the upstream or the caller can report terminal work: retain the token, request ID, and registered fence from the same admission, then send them to the configurable completion path:

```json
{
  "requestId": "REQUEST_ID",
  "registrationToken": "TOKEN_FROM_ADMISSION",
  "fence": {
    "incarnation": "LEASE_INCARNATION",
    "epoch": 1
  },
  "outcome": "completed"
}
```

The default completion path is `/_gpu-workload-supervisor/v1/work/finish`. A successful terminal update returns HTTP 204. Completion without the matching token is rejected for newly admitted work. Older registrations created before token support remain compatible with tokenless completion until their lease fence rotates. Proxy, upstream, client, and process failures leave work incomplete for explicit reconciliation.

Completed registrations and their transition snapshot links are pruned after 30 days by default (`-completed-work-retention` changes the period). Active work and snapshots of transitions still in progress are retained. After pruning, a request ID can be reused, including under the same lease fence; the registration token prevents an old completion from finishing the new admission. Legacy tokenless registrations under the current fence are kept until the fence rotates.

Safe methods are forwarded by default. Unclassified mutating routes fail closed. Required non-execution mutations must be explicitly configured as passthrough routes.

The listener is restricted to loopback because explicit user ownership bypasses supervisor lease registration. External exposure and requester authentication belong to the deployment boundary.

The proxy is content-blind. Content inspection and domain policy belong to the caller.

## Distribution

Tagged releases contain checksummed Linux artifacts for amd64 and arm64. Artifacts include:

- `gpu-mode`
- `gpu-workload-proxy`

Configuration is supplied at deployment time. This repository does not contain environment-specific service names, paths, identities, ports, network names, or deployment automation.

## Trust boundary

Processes sharing the supervisor identity are trusted. A compromised process with the same operating-system permissions can bypass advisory locks, state files, and service control. Strong isolation requires separate service identities and operating-system enforced permissions.

Execution requests must not have a route that bypasses the gate while supervisor ownership is active.

Repository checks and settings: [repository controls](docs/REPOSITORY-CONTROLS.md). Dependabot: [policy](docs/DEPENDABOT.md). Restores: [procedure](docs/RESTORING.md). Releases: [releasing](docs/RELEASING.md).
