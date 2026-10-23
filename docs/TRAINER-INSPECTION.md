# Trainer binding inspection

`gpu-setup discover` includes a separate `trainers` array for explicit trainer
profiles in the current catalog or pending activation. Existing application
discovery is unchanged. An empty array means no explicit trainer bindings were
available, not that no training software is installed.

Each result identifies the workload and unit, reports `bindingStatus`, and keeps
`qualificationStatus: "unqualified"`. A `verified` binding means the same
executable, recipe, exact unit source, loaded command/environment and physical
GPU identity checks used before training passed. It does not approve the Python
environment, establish measured capacity, test GPU kernels or authorize launch.
Failures remain visible with generic diagnostic guidance, without subprocess
output or lifecycle credentials.

The setup backend also exposes `InspectTrainer(ctx, request, workload)` for an
explicit draft setup request. It validates the request before inspecting the
selected binding and never applies it. A new binding must supply the complete
version 4 catalog trainer profile described in [TRAINING.md](TRAINING.md),
including measured `requiredMiB`, GPU UUID, unit/executable/recipe paths and
digests, and private lifecycle paths. Inspection does not infer these values
from a service name or read the job envelope to discover them.

Inspection performs file and loaded-unit observations only. It does not execute
the trainer or preparation command, start/stop services, install dependencies,
write lifecycle files, change the catalog or enable a preset. All launch checks
run again at training start. The training UI remains separate in #456.
