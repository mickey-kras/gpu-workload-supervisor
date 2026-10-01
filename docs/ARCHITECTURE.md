# State and responsibility

[Documentation](README.md) | [Repository](../README.md)

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

## External control and clients

The transport-neutral [external control contract](external-control.md) lets
authenticated, policy-limited automation inspect state and conditionally request
an approved supervisor workload. It provides no listener or deployment exposure.
Broker workflows and execution/completion remain subject to the proxy contract.

The [dashboard and local desktop client plan](CLIENTS.md) records future
packaging, authority boundaries, and mandatory acceptance gates. It adds no
running client; both clients remain deferred and outside initial deployment.


## Updating the overview

The repository README uses one overview with light/dark variants. Its topology follows `internal/proxy`, `internal/supervisor`, `internal/store`, and `internal/runtime`. Regenerate both maintained SVG assets with:

```sh
python3 scripts/architecture-overview.py
```

The controller is separate from the request path; it manages runtime transitions and writes the durable state the proxy uses for admission. Do not hand-edit the generated SVG files.
