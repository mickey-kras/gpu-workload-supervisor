# GPU Workload Supervisor

[![PR validation](https://github.com/mickey-kras/gpu-workload-supervisor/actions/workflows/pr-validation.yml/badge.svg)](https://github.com/mickey-kras/gpu-workload-supervisor/actions/workflows/pr-validation.yml)
[![CodeQL](https://github.com/mickey-kras/gpu-workload-supervisor/actions/workflows/codeql.yml/badge.svg)](https://github.com/mickey-kras/gpu-workload-supervisor/actions/workflows/codeql.yml)
[![Aislop](https://badges.scanaislop.com/score/mickey-kras/gpu-workload-supervisor.svg)](https://scanaislop.com/mickey-kras/gpu-workload-supervisor)
[![main validation and SonarQube](https://github.com/mickey-kras/gpu-workload-supervisor/actions/workflows/main.yml/badge.svg?branch=main)](https://github.com/mickey-kras/gpu-workload-supervisor/actions/workflows/main.yml?query=branch%3Amain)
[![Go coverage gate](https://img.shields.io/badge/Go%20coverage-at%20least%2075%25%20(CI--gated)-yellow)](https://github.com/mickey-kras/gpu-workload-supervisor/actions/workflows/pr-validation.yml)
[![MIT license](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Crash-safe GPU workload supervision for local AI runtimes.

## Scope

The supervisor owns:

- GPU owner and workload state
- admission control
- lease fencing
- transition journaling
- boot recovery state

It does not own job execution, prompt policy, model APIs, or runtime data.

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

A lease fence combines a store-incarnation UUID with a monotonic epoch. Restore or rebuild rotates the incarnation. A transition rotates the epoch before work is drained.

SQLite uses WAL mode, full synchronous writes, foreign keys, and one database connection. Transition side effects are recorded as intent and observation events.

## Development

```sh
go test ./...
go vet ./...
```

## Local CLI

Runtime identity is deployment configuration. Unit names, endpoints, and a measured post-release GPU-memory threshold are required:

```sh
gpu-mode \
  -text-unit TEXT.service \
  -media-unit MEDIA.service \
  -text-health-url http://127.0.0.1:PORT/health \
  -media-health-url http://127.0.0.1:PORT/ \
  -media-release-url http://127.0.0.1:PORT/free \
  -gpu-index 0 \
  -release-max-used-mib LIMIT \
  -nvidia-smi /ABSOLUTE/PATH/nvidia-smi \
  -systemctl /ABSOLUTE/PATH/systemctl \
  status|reconcile|recover|text|media|idle
```

Defaults:

- state: `$XDG_STATE_HOME/gpu-workload-supervisor/state.db` or `~/.local/state/gpu-workload-supervisor/state.db`
- health request timeout: 10 seconds
- runtime action timeout: 2 minutes
- drain timeout: 5 minutes
- readiness timeout: 5 minutes
- rollback timeout: 2 minutes
- failure finalization timeout: 10 seconds
- poll interval: 250 milliseconds

The state directory must be private and owned by the current user. Commands use an exclusive file lock. Opening the store applies pending migrations. `status` persists a closed error state if runtime observation violates the single-GPU invariant.

ComfyUI process availability is separate from media GPU ownership. Entering media mode stops text inference and keeps ComfyUI available. Before starting text inference, the supervisor calls the media-release endpoint, then polls `nvidia-smi` until reported GPU memory is at or below the deployment threshold.

Boot reconciliation never resumes media work. Interrupted transitions and latched errors require explicit `recover`.

The initial adapter uses trusted `systemctl` and `nvidia-smi` executables. Typed D-Bus and NVML adapters may replace subprocess polling after deployment benchmarks.

## Trust boundary

Processes sharing the supervisor Unix identity are trusted. ComfyUI execution must pass through a lease-enforcing gate; direct `/prompt` access must remain blocked in supervisor mode. A compromised same-UID runtime can bypass advisory locks, state files, and user-service control. Remote control requires separate supervisor and runtime service identities with OS-enforced permissions.

Remote control, authorization, UI, Job Broker implementation, and host-specific deployment remain outside this repository slice.

Repository checks and setup: [onboarding](docs/ONBOARDING.md). Dependabot: [policy](docs/DEPENDABOT.md). Source releases: [releasing](docs/RELEASING.md).
