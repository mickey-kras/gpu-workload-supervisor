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

The controller is separate from the request path; it manages runtime transitions and writes the durable state the proxy uses for admission. The proxy also writes that state when it admits and finishes work.

## State ownership

Four executables open, migrate, and write `state.db`:

- `gpu-mode` records workload transitions.
- `gpu-workload-proxy` writes admission and completion records (`AdmitWorkToken`, `FinishWorkToken`).
- `gpu-operator` runs the embedded supervisor transitions behind local requests.
- `gpu-setup` commits the accepted catalog.

Opening the database from any of them can apply pending migrations; back up before changing binaries.

## Clients

The shipped local client is the GNOME Shell extension in `clients/gnome/`,
packaged per [desktop installation](DESKTOP.md); its contract is the client
half of the [operator protocol](OPERATOR.md#gnome-shell-extension-client).


## Updating the overview

The repository README uses one overview with light/dark variants. Its topology follows `internal/proxy`, `internal/supervisor`, `internal/store`, `internal/runtime`, `internal/control`, and `internal/operator`. Regenerate both maintained SVG assets with:

```sh
python3 scripts/architecture-overview.py
```

Do not hand-edit the generated SVG files.
