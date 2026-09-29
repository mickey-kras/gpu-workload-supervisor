# GPU Workload Supervisor

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

```sh
go run ./cmd/gpu-mode status
go run ./cmd/gpu-mode reconcile
go run ./cmd/gpu-mode text
go run ./cmd/gpu-mode media
go run ./cmd/gpu-mode idle
```

The controller operates on systemd user units. Workload transitions close admission, snapshot active work, wait for the snapshot to drain, journal runtime actions, and verify observed state plus health before reopening admission.

Remote control, authorization, UI, Job Broker implementation, and host-specific deployment remain outside this repository slice.
