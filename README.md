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

Runtime adapters and service control are intentionally outside the initial state-engine slice.
