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

## Integration boundaries

| Component | Responsibility |
| --- | --- |
| Lifecycle controller | GPU ownership, admission state, fencing, transitions and verified release |
| Execution proxy and adapters | Forward requests, register admitted work and correlate terminal evidence |
| External job broker | Workflow steps, retries, checkpoints, cancellation and results |
| External inference gateway | Request/response policy, authentication and content validation |
| External model ingestion tools | Artifact scanning, reports, signing and promotion |

The controller receives control metadata, not prompts or responses. The packaged
proxy does receive and forward workload payloads, and may validate bounded
protocol envelopes or exact-body authorization digests; it does not make semantic
content-policy decisions. Durable demand records are admission evidence, not a
workflow scheduler or permission to replay ambiguous execution.

Local setup does not install a broker, gateway or scanner. Those integrations
must preserve admission, terminal-evidence and direct-runtime isolation contracts.
Trainer adapters own recipes and checkpoint contents; the controller owns their
finite-job lifecycle. See [training](TRAINING.md).

## Planned model attestation

[#585](https://github.com/mickey-kras/gpu-workload-supervisor/issues/585) owns
external scan/report and attestation preparation;
[#583](https://github.com/mickey-kras/gpu-workload-supervisor/issues/583) owns
verification before supervised use and UI status. Neither is implemented.

Plan adoption of [Microsoft's LLM Backdoor Scanner](https://github.com/microsoft/llm-backdoor-scanner)
as one qualified ingestion check, with additional tools selected later. Scanning
may execute an untrusted model: use isolated staging before serving approval,
outside normal activation. Unsupported formats, skipped checks and errors never
count as a pass.

Signed evidence binds the exact artifact inventory, scanner versions/configuration,
results and policy. Signing keys stay outside the supervisor. The execution gate
verifies evidence and artifact identity without running scanners. Converted,
quantized or adapter-composed artifacts need their own identity and assessment;
a source-model result does not automatically qualify a derivative.

The planned UI warns when required checks have not passed, distinguishes findings
from unavailable checks and integrity failures, and exposes report details.
Use "Verified attestation", never "Safe model", with:
"These checks are a defensive component, not comprehensive backdoor or vulnerability detection."
Advisory/enforce policy and explicit exceptions follow #583; invalid signatures
or artifact mismatches are always rejected.

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
