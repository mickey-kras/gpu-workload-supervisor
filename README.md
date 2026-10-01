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

Runtime identities, health endpoints, release behavior, and measured capacity requirements are deployment configuration:

```sh
gpu-mode \
  -text-unit TEXT.service \
  -media-unit MEDIA.service \
  -text-health-url http://127.0.0.1:PORT/health \
  -media-health-url http://127.0.0.1:PORT/health \
  -media-stop-mode stop-service \
  -text-cgroup /DEPLOYMENT/CGROUP/TEXT.service \
  -media-cgroup /DEPLOYMENT/CGROUP/MEDIA.service \
  -systemctl /ABSOLUTE/PATH/systemctl \
  status|reconcile|recover|resolve-work|text|media|idle
```

`stop-service` verifies both units are `inactive/dead` and their configured
cgroup v2 subtrees have `cgroup.events` `populated 0`, which includes descendants.
A removed workload cgroup is also released; missing events in an existing group,
unreadable or malformed evidence, mismatched systemd metadata, and surviving
children fail closed. The UI is unavailable outside media mode.

The default `-media-stop-mode unload` still sends the configured release request
and leaves the UI alive. **Live-media unload cannot currently be verified:** HTTP
2xx does not prove the runtime has drained work and released models/resources, and
no supported runtime-specific verifier is implemented. Release therefore fails
closed with an actionable error. Use `stop-service`, or explicitly stop media
before recovery. Stopped media uses cgroup verification in either policy, including
ownership changes and work resolution. Stopped text is always verified. Readiness
and verify-only recovery also verify the opposing workload's release. Switching
text or idle to media may retain the destination UI after text release is proven;
it does not require unloading the destination itself.

Configure both cgroup paths from deployment knowledge of the actual systemd units
(e.g. inspect `systemctl --user show --property=ControlGroup -- UNIT.service` while
the unit is running). Paths are absolute **within** `/sys/fs/cgroup`, canonical,
non-root, distinct, and non-overlapping. A nonempty systemd `ControlGroup` must
match; an empty property after shutdown uses the explicit configuration, never an
in-memory PID or path cache. Keep configuration consistent across CLI invocations
and update it if unit placement changes. Blank metadata alone is not evidence.
All workload workers must remain in their configured subtree. Use the host unified
cgroup v2 root mounted at `/sys/fs/cgroup`, alongside host user systemd. The same
manager's loaded, active root `-.slice` must report an existing, readable cgroup
that strictly contains both workload paths. This anchor validates manager-to-mount
mapping without privileged access to PID 1. Subtree/nested mounts and container
mappings are unsupported; missing or ambiguous manager anchors fail closed.

**Migration:** remove `-release-max-used-mib` and configure both cgroup paths.
Every explicit use of the old flag, including `=0`, is rejected before opening
state. Do not replace it with a larger threshold or a learned idle baseline.
Release never queries total GPU memory or GPU process accounting: unrelated desktop
allocations, PID reuse, and unavailable accounting cannot change the result.
Cgroup evidence proves workload processes are gone, not that asynchronous driver
cleanup has finished.

Optional pre-start capacity checks use `-text-required-mib` and/or
`-media-required-mib` (measured target requirements) plus
`-capacity-headroom-mib`. A zero target requirement disables that target's check.
Configure `-nvidia-smi /ABSOLUTE/PATH/nvidia-smi` and `-gpu-index` when using these
checks. Available `memory.free` must meet requirement plus headroom; delayed driver
cleanup or other users can cause a distinct capacity error after successful
release. Capacity failures keep admission closed and require explicit recovery;
repair capacity before retrying. A capacity snapshot cannot reserve GPU memory.

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

Failed and interrupted operations retain the last committed owner and require explicit recovery. Stopped user work is never restarted by rollback. For supervisor ownership, use `recover`. For user ownership, inspect the runtime and run `gpu-mode [runtime flags] -target text|media|idle recover-user` with the workload you intend to keep. This command verifies the target without starting or stopping runtimes, preserves user ownership, and keeps supervisor admission closed. An idle recovery verifies release using the configured policy. If verification fails, repair or stop runtimes explicitly and retry. `reconcile` never automatically takes control from the user. Every transfer, user switch, and recovery rotates the fence and records durable source/target ownership and transition outcomes.

If admitted work cannot report completion (for example, forwarding failed before either party received its registration token), stop every `gpu-workload-proxy` instance for this state file and wait for shutdown. Run `gpu-mode` with the normal runtime flags plus `-resolve-reason 'operator incident reference' resolve-work`. Proxy instances hold a shared lifetime lock; `resolve-work` refuses to run until they are gone, then closes admission, rotates the fence, stops both runtimes, verifies stopped units and empty workload cgroups, and atomically marks unfinished work abandoned with an audit record. Do not use an older proxy binary without the lifetime lock during this operation. If any stop or verification fails, work remains unfinished and admission stays closed. Run `recover` after successful resolution, then restart proxies and switch workload as needed. This operation terminates all running work, so use it only after investigating the orphaned requests.

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
