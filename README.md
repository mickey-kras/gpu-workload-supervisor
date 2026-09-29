# GPU Workload Supervisor

[![PR validation](https://github.com/mickey-kras/gpu-workload-supervisor/actions/workflows/pr-validation.yml/badge.svg)](https://github.com/mickey-kras/gpu-workload-supervisor/actions/workflows/pr-validation.yml)
[![CodeQL](https://github.com/mickey-kras/gpu-workload-supervisor/actions/workflows/codeql.yml/badge.svg)](https://github.com/mickey-kras/gpu-workload-supervisor/actions/workflows/codeql.yml)
[![Aislop](https://badges.scanaislop.com/score/mickey-kras/gpu-workload-supervisor.svg)](https://scanaislop.com/mickey-kras/gpu-workload-supervisor)
[![main validation and SonarQube](https://github.com/mickey-kras/gpu-workload-supervisor/actions/workflows/main.yml/badge.svg?branch=main)](https://github.com/mickey-kras/gpu-workload-supervisor/actions/workflows/main.yml?query=branch%3Amain)
[![Go coverage gate](https://img.shields.io/badge/Go%20coverage-at%20least%2053%25-yellow)](https://github.com/mickey-kras/gpu-workload-supervisor/actions/workflows/pr-validation.yml)
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

Runtime integration and service control are developed separately from the initial state engine. The release workflow currently packages the source tree.

Repository checks and setup: [onboarding](docs/ONBOARDING.md). Dependabot: [policy](docs/DEPENDABOT.md). Source releases: [releasing](docs/RELEASING.md).
