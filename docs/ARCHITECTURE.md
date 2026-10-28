# State and responsibility

[Documentation](README.md) | [Repository](../README.md)

## Boundaries

| Part | Responsibility |
| --- | --- |
| Controller | Ownership, desired state, admission, fencing, transitions, journal, recovery and verified GPU release |
| Proxy and adapters | Route gating, forwarding, optional requester authentication, work registration and terminal evidence |
| External broker | Workflow steps, retries, checkpoints, cancellation and results |
| External gateway | Gateway authentication, request and response policy and content validation |
| External ingestion tools | Model scanning, reports, signing and promotion |

- Controller: control metadata only. No prompts or responses.
- Proxy: payload forwarding, bounded protocol validation and exact-body authorization. Optional [requester authentication](REQUESTER-INGRESS.md) before workflow authorization. No semantic content policy.
- Demand records: admission evidence. No workflow scheduling or replay of ambiguous execution.
- Integrations: keep admission, terminal evidence and runtime-bypass prevention.
- Setup: no broker, gateway or scanner installation.
- [Trainer adapters](TRAINING.md): recipes and checkpoint contents. Controller: finite-job lifecycle.

## Model checks

The supervisor does not check models yet.
[#585](https://github.com/mickey-kras/gpu-workload-supervisor/issues/585) plans a scan of each LLM before its first use.
[#583](https://github.com/mickey-kras/gpu-workload-supervisor/issues/583) plans to show the result and to stop models that did not pass.
These checks are a defensive layer. They do not prove that a model is safe.

## State model

New stores need reconciliation:

```text
owner=supervisor
desiredWorkload=idle
activeWorkload=unknown
phase=reconciling
health=healthy
admission=closed
```

- Fence: store-incarnation UUID + monotonic epoch.
- Fresh store: new incarnation. Explicit restore: rotate incarnation. Restart: preserve it.
- Transition: rotate epoch before draining.
- SQLite: WAL, full synchronous writes, foreign keys, one connection.
- Journal: side-effect intent and observation.
- Controller writes transitions. Proxy writes admission and completion.

## State writers

| Executable | Writes to `state.db` |
| --- | --- |
| `gpu-mode` | Workload transitions |
| `gpu-workload-proxy` | Admission and completion |
| `gpu-operator` | Embedded supervisor transitions |
| `gpu-setup` | Accepted catalog |

Any writer can apply migrations when opening the database. Make a backup before you change binaries.

## Clients

GNOME extension: `clients/gnome/`.
[Installation](DESKTOP.md) |
[Protocol](OPERATOR.md#gnome-shell-extension-client).

## Overview assets

Regenerate the README's light and dark SVGs. Do not edit them manually:

```sh
python3 scripts/architecture-overview.py
```

Keep topology aligned with `internal/{proxy,supervisor,store,runtime,control,operator}`.
