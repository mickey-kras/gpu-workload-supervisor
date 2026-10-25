# State and responsibility

[Documentation](README.md) | [Repository](../README.md)

## Boundaries

| Component | Responsibility |
| --- | --- |
| Controller | Ownership, desired state, admission, fencing, transitions, journal, recovery and verified GPU release |
| Proxy/adapters | Route gating, forwarding, optional requester authentication, work registration and terminal evidence |
| External broker | Workflow steps, retries, checkpoints, cancellation and results |
| External gateway | Gateway authentication, request/response policy and content validation |
| External ingestion tools | Model scanning, reports, signing and promotion |

- Controller: control metadata only; no prompts or responses.
- Proxy: payload forwarding, bounded protocol validation and exact-body authorization; optional [requester authentication](REQUESTER-INGRESS.md) before workflow authorization; no semantic content policy.
- Demand records: admission evidence; no workflow scheduling or replay of ambiguous execution.
- Integrations: preserve admission, terminal evidence and runtime-bypass prevention.
- Setup: no broker, gateway or scanner installation.
- [Trainer adapters](TRAINING.md): recipes and checkpoint contents; controller: finite-job lifecycle.

## Model attestation

The [attestation contract](MODEL-ATTESTATION.md) defines the envelope, signing bytes and policy.
The producer is [`gpu-model-seal`](MODEL-SEAL.md) ([#585](https://github.com/mickey-kras/gpu-workload-supervisor/issues/585)):
scan, report and attestation preparation. The gate does not exist yet.
[#583](https://github.com/mickey-kras/gpu-workload-supervisor/issues/583):
pre-execution verification, policy and UI.

- Adopt [Microsoft's LLM Backdoor Scanner](https://github.com/microsoft/llm-backdoor-scanner) after qualification; select additional tools later.
- Scan in isolated staging before serving approval, never during activation. Scans may execute untrusted models.
- Bind signatures to exact artifact inventory, scanner versions/configuration, results and policy.
- Keep signing keys and scanners outside the supervisor; verify evidence and artifact identity before model loading or execution.
- Assess converted, quantized and adapter-composed artifacts separately.
- Reject invalid signatures and artifact mismatches. Advisory/enforce rules and explicit exceptions: #583.
- Warn on missing, unsupported, skipped or failed checks; distinguish findings from integrity failures.
- UI: "Verified attestation", never "Safe model"; report details available.

UI notice: "These checks are a defensive component, not comprehensive backdoor or vulnerability detection."

## State model

New stores require reconciliation:

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
- Controller writes transitions; proxy writes admission and completion.

## State writers

| Executable | Writes to `state.db` |
| --- | --- |
| `gpu-mode` | Workload transitions |
| `gpu-workload-proxy` | Admission and completion |
| `gpu-operator` | Embedded supervisor transitions |
| `gpu-setup` | Accepted catalog |

Any writer can apply migrations when opening the database. Back up before changing binaries.

## Clients

GNOME extension: `clients/gnome/`.
[Installation](DESKTOP.md) |
[Protocol](OPERATOR.md#gnome-shell-extension-client).

## Overview assets

Regenerate the README's light/dark SVGs; do not edit them manually:

```sh
python3 scripts/architecture-overview.py
```

Keep topology aligned with `internal/{proxy,supervisor,store,runtime,control,operator}`.
