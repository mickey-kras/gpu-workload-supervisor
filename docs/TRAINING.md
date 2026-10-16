# Finite training jobs

Training uses a separate catalog version 4 binding. Catalog versions 1 and 2
remain supported; version 3 remains reserved. Inference `nativeModel` bindings
cannot launch training.

The trainer adapter owns its recipe, datasets, metrics, checkpoints, evaluation
and export. All GPU work must remain inside its systemd unit's recursive cgroup.
The supervisor stores lifecycle identity and terminal evidence only. It does not
queue jobs or resume interrupted training automatically.

## Adapter contract, version 1

A trainer profile uses `adapter: "systemd"`, `bootPolicy: "stop-to-idle"`, a
measured nonzero `requiredMiB`, and no health/release endpoint, native model or
application launch binding. Its `trainer` object contains:

| Field | Meaning |
| --- | --- |
| `version` | `1` |
| `gpuUUID` | Full physical GPU UUID |
| `launchFile`, `launchSHA256` | Exact unit source and SHA256 |
| `executable`, `executableSHA256` | Trusted root-owned adapter and SHA256 |
| `recipeFile`, `recipeSHA256` | Trusted local recipe and SHA256 |
| `jobFile` | Private lifecycle envelope written before launch |
| `terminalFile` | Trusted terminal evidence written by the adapter |

All paths must be distinct, absolute, clean literal paths without links or shell
expansion. Lifecycle output files cannot overlap another trainer binding. The
job directory must already exist with trusted ownership and permissions. Keep
external service management paused while a supervisor job is unfinished.

The accepted unit source has exactly this form, with the bound values substituted
and a final newline. Additional directives and drop-ins are unsupported:

```ini
[Unit]
Description=Supervised finite training
[Service]
Type=exec
Restart=no
KillMode=control-group
RemainAfterExit=yes
Environment=CUDA_VISIBLE_DEVICES=GPU-...
ExecStart=/trusted/trainer-adapter --job-file /private/training/job.json
```

The adapter reads the version 1 JSON envelope containing `job`, `recipeFile` and
`terminalFile`. `job` includes ID, completion token, fence, workload, accepted
catalog revision and recipe SHA256. The adapter must validate this envelope and
the recipe digest before GPU-backed preparation. Its terminal file is one strict
JSON object, at most 4096 bytes, with `version: 1`, `id`, `token`, `fence`,
`recipeSHA256`, `invocationId` (systemd's `INVOCATION_ID`) and `outcome`.
Supported outcomes are `completed`, `failed`, `cancelled` and `interrupted`.
Write evidence atomically with private permissions. Never print the token.

`completed` means every requested phase, including evaluation/export, completed.
The unit retains an exited invocation until supervisor cleanup. A zero process exit is insufficient. A fenced systemd OOM, exit-code, signal or
core-dump failure supplies failure evidence without requiring a surviving adapter.
The supervisor still stops the unit, verifies recursive cgroup emptiness, verifies
no GPU compute processes remain, and checks capacity before releasing admission.

Backend adapters and their environment qualification are separate integrations.
This contract does not claim checkpoint or resume support for an arbitrary trainer.
The [LLM candidate preparation tool](LLM-TRAINING-CANDIDATE.md) audits local inputs
and generates an unqualified Axolotl configuration; it does not implement this
executable adapter contract or enable a training profile.

## Commands

Use the usual `gpu-mode` state/runtime flags before the command:

```sh
gpu-mode -target trainer -training-job unique-job-id training-start
gpu-mode -training-job unique-job-id training-status
gpu-mode -training-job unique-job-id training-reconcile
gpu-mode -training-job unique-job-id training-recover
```

Start validates the catalog and output paths, then durably reserves a unique
request ID before draining opposing work. This `preparing` claim blocks competing
requester demand and work registrations but is not GPU admission. After verified
idle release, the matching claim is consumed atomically with job registration and
single-use launch intent. IDs cannot be reused, including interrupted preparations.
A crash during preparation is visible in status. Explicit recovery records it as
`interrupted` without stopping unrelated inference or launching the trainer. Failed
handoffs retire the claim when possible; otherwise preparation remains recoverable.
Routine switches, auto-idle, catalog replacement and generic recovery cannot
discard the admission. Status redacts the completion token.

Reconcile reads evidence and never starts the trainer. A validated terminal outcome
is durably bound to the job before cleanup. Failed cleanup/finalization can retry
without losing that outcome; admission remains pending until release is verified.
Missing, stale or ambiguous evidence keeps the job pending. After a launch/controller crash, `training-recover`
explicitly stops the configured trainer, verifies release and records
`interrupted` when no verified terminal outcome was previously persisted; otherwise
it preserves that outcome. Recovery never retries the launch. If stop or release verification fails,
the job remains pending and recovery can be retried. Start a new job ID only after
the prior job is terminal. A resume integration must create a new job and fence.
