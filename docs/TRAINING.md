# Finite training jobs

Training uses a separate catalog version 4 binding. Catalog versions 1 and 2
remain supported. Version 3 remains reserved. Inference `nativeModel` bindings
cannot launch training.

The trainer adapter owns its recipe, datasets, metrics, checkpoints, evaluation
and export. All GPU work must remain inside its systemd unit's recursive cgroup.
The supervisor stores only lifecycle identity, terminal evidence and the memory
peak of the trainer unit. It does not queue jobs or resume interrupted training
automatically.

## Adapter contract, version 1

A trainer profile uses `adapter: "systemd"`, `bootPolicy: "stop-to-idle"`, a
measured nonzero `requiredMiB`, and no health or release endpoint, native model or
application launch binding. Its `trainer` object contains:

| Field | Meaning |
| --- | --- |
| `version` | `1` |
| `gpuUUID` | Full physical GPU UUID |
| `launchFile`, `launchSHA256` | Exact unit source and SHA256 |
| `executable`, `executableSHA256` | Adapter qualified by shared executable trust checks and SHA256 |
| `recipeFile`, `recipeSHA256` | Trusted local recipe and SHA256 |
| `jobFile` | Private lifecycle envelope written before launch |
| `terminalFile` | Trusted terminal evidence written by the adapter |

All paths must be distinct, absolute, clean literal paths without shell expansion.
You or root must own the adapter and its traversed ancestors.
They must not allow world write access. The validator resolves and checks symlinks.

The single-admin host policy trusts members
of your own groups. It allows group write only
for groups you belong to. Lifecycle output
files cannot overlap another trainer binding. The
job directory must already exist with trusted ownership and permissions. Keep
external service management paused while a supervisor job remains unfinished.

The accepted unit source has exactly this form, with the bound values substituted
and a final newline. The validator does not support more directives or drop-ins:

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
JSON object, at most 64 KiB (65536 bytes), with `version: 1`, `id`, `token`, `fence`,
`recipeSHA256`, `invocationId` (systemd's `INVOCATION_ID`) and `outcome`.

Supported outcomes are `completed`, `failed`, `cancelled` and `interrupted`.
Job IDs follow the shared request ID contract: at most 8192 UTF-8 bytes and no
control characters. The terminal limit allows the full ID even when JSON escaping
expands each ID byte to six bytes, plus the remaining receipt fields.
Write evidence atomically with private permissions.

Never print the token.

`completed` means every requested phase, including evaluation and export, completed.
The unit retains an exited invocation until supervisor cleanup. A zero process exit is insufficient. A fenced systemd OOM, exit-code, signal or
core-dump failure supplies failure evidence without requiring a surviving adapter.
The supervisor still stops the unit, verifies recursive cgroup emptiness, verifies
no GPU compute processes remain, and checks capacity before releasing admission.

Backend adapters and their environment qualification are separate integrations.
This contract does not claim checkpoint or resume support for an arbitrary trainer.
The [LLM tool for candidate preparation](LLM-TRAINING-CANDIDATE.md) audits local inputs
and generates an unqualified Axolotl configuration. It does not supply this
executable adapter contract or enable a training profile. The separate
[experimental LLM runner](LLM-TRAINING-RUNNER.md) implements that execution
boundary without qualifying a runtime or enabling a profile.

Read-only [trainer inspection](TRAINER-INSPECTION.md) exposes explicit binding
validation for discovery and future setup. Follow the
[LLM qualification guide](LLM-TRAINING-QUALIFICATION.md) for candidate recipe
materialization and private host evidence. Successful inspection is not GPU qualification.

## Commands

Use the usual `gpu-mode` state and runtime flags before the command:

```sh
gpu-mode -target trainer -training-job unique-job-id training-start
gpu-mode -training-job unique-job-id training-status
gpu-mode -training-job unique-job-id training-reconcile
gpu-mode -training-job unique-job-id training-recover
```

The start command validates the catalog and output paths, then durably reserves a unique
request ID before draining opposing work. This `preparing` claim blocks competing
requester demand and work registrations but is not GPU admission. After verified
idle release, one transaction consumes the matching claim and records job registration and
single-use launch intent. The supervisor rejects reused IDs, including IDs of interrupted preparations.

A crash during preparation is visible in status. Explicit recovery records it as
`interrupted` without stopping unrelated inference or launching the trainer. Failed
handoffs retire the claim when possible. Otherwise preparation remains recoverable.

Routine switches, auto-idle, catalog replacement and generic recovery cannot
discard the admission. Status redacts the completion token.

Reconcile reads evidence and never starts the trainer. Before cleanup, the supervisor
durably binds a validated terminal outcome to the job. Failed cleanup or finalization can retry
without losing that outcome. Admission remains pending until the supervisor verifies release.

Missing, stale or ambiguous evidence keeps the job pending. After a launch or controller crash, `training-recover`
explicitly stops the configured trainer, verifies release and records
`interrupted` when no verified terminal outcome exists in durable state. Otherwise
it preserves that outcome. Recovery never retries the launch.

If stop or release verification fails,
the job remains pending. You can retry recovery. Start a new job ID only after
the prior job is terminal. A resume integration must create a new job and fence.

### Memory peak

Before cleanup stops the trainer unit, reconcile and recovery read the memory
peak of the unit. The job record keeps the largest value as `memoryPeakBytes`,
and status shows it. This recording is best effort: a missing peak or a failed
write never blocks the stop or the cleanup. `memoryPeakBytes` is a field of the
job record. The terminal file contract does not change.

systemd can report the unit cgroup for the invocation of the job. Then the
supervisor keeps the larger of the cgroup `memory.peak` and the `MemoryPeak`
property. Otherwise it reads only the `MemoryPeak` property. The kernel and
systemd measure the value, not the trainer. The value includes the page cache,
but only of processes in the unit cgroup. Trainer code can move processes out of
that cgroup, so the value is a lower bound and not proof against changes.

Reconcile reads the value after the trainer writes its outcome. Recovery reads
it before it stops the unit. When recovery stops a running trainer, the value is
the cgroup peak up to that time. When the unit already exited or failed, systemd
usually removed the cgroup, and the value comes from the `MemoryPeak` property.

A value for every outcome needs systemd 256 or later. The systemd source shows
these facts for the supported versions (see [the desktop requirements](DESKTOP.md)):

- systemd 252 has no `MemoryPeak` property. systemd 255 adds it.
- systemd 255 keeps the value only if something reads it before it removes the
  cgroup. Thus it usually gives no value after the unit exits.
- systemd 256 and later, 259 included, keep the value when they remove the cgroup.

Use the systemd version to decide. Run `systemctl --version`. On systemd 256 or
later, `memoryPeakBytes` has a value for every outcome. On earlier versions, the
supervisor does not sample during the run. For qualification, the operator reads
the `memory.peak` file of the unit cgroup directly during every run, not through
`systemctl`.

To confirm, use a unit that nothing queried during its run, because a query
during the run can make systemd 255 keep a value. After the unit exits, run
`systemctl --user show -p MemoryPeak -- <trainer-unit>.service`. A decimal byte
count shows that systemd kept the value. Any other output shows that it did not.
Examples are `[not set]`, `infinity`, `18446744073709551615`, an empty value and
no `MemoryPeak=` line.

These are some conditions that make the field absent:

- Your systemd version does not keep `MemoryPeak`.
- The unit cgroup has no memory controller.
- The unit has another invocation.
- The read or the write failed or timed out. Recording is best effort and leaves
  no log.

For qualification, follow [the source order](LLM-TRAINING-QUALIFICATION.md#private-host-inputs-and-evidence).

