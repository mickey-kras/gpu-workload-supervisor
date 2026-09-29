# GPU Workload Supervisor

[![PR validation](https://github.com/mickey-kras/gpu-workload-supervisor/actions/workflows/pr-validation.yml/badge.svg)](https://github.com/mickey-kras/gpu-workload-supervisor/actions/workflows/pr-validation.yml)
[![CodeQL](https://github.com/mickey-kras/gpu-workload-supervisor/actions/workflows/codeql.yml/badge.svg)](https://github.com/mickey-kras/gpu-workload-supervisor/actions/workflows/codeql.yml)
[![Aislop](https://badges.scanaislop.com/score/mickey-kras/gpu-workload-supervisor.svg)](https://scanaislop.com/mickey-kras/gpu-workload-supervisor)
[![main validation and SonarQube](https://github.com/mickey-kras/gpu-workload-supervisor/actions/workflows/main.yml/badge.svg?branch=main)](https://github.com/mickey-kras/gpu-workload-supervisor/actions/workflows/main.yml?query=branch%3Amain)
[![Go coverage gate](https://img.shields.io/badge/Go%20coverage-at%20least%2090%25%20(CI--gated)-brightgreen)](https://github.com/mickey-kras/gpu-workload-supervisor/actions/workflows/pr-validation.yml)
[![MIT license](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

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
  status|reconcile|recover|text|media|idle
```

The state directory must be private and owned by the current user. Commands use an exclusive file lock. Opening the store applies pending migrations. Boot reconciliation does not resume incomplete work. Interrupted transitions and latched errors require explicit recovery.

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

Control headers are removed before forwarding. Work is registered before forwarding and remains active after submission. The caller must send a terminal `completed` or `abandoned` outcome to the configurable completion path with the request ID and registered fence:

```json
{
  "requestId": "REQUEST_ID",
  "fence": {
    "incarnation": "LEASE_INCARNATION",
    "epoch": 1
  },
  "outcome": "completed"
}
```

The default completion path is `/_gpu-workload-supervisor/v1/work/finish`. A successful terminal update returns HTTP 204. Proxy, upstream, client, and process failures leave work incomplete for explicit reconciliation.

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

Repository checks and setup: [onboarding](docs/ONBOARDING.md). Dependabot: [policy](docs/DEPENDABOT.md). Restores: [procedure](docs/RESTORING.md). Releases: [releasing](docs/RELEASING.md).
