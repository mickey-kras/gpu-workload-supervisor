# Trainer binding inspection

`gpu-setup discover` includes a separate `trainers` array for explicit trainer
profiles in the current catalog or pending activation. Existing application
discovery stays unchanged. An empty array means no explicit trainer bindings were
available, not that the host has no training software.

Each result identifies the workload and unit, reports `bindingStatus`, and keeps
`qualificationStatus: "unqualified"`. A `verified` binding means the same
executable, recipe, exact unit source, loaded command and environment and physical
GPU identity checks used before training passed. It does not approve the Python
environment, prove measured capacity, test GPU kernels or authorize launch.
Failures remain visible with generic diagnostic guidance, without subprocess
output or lifecycle credentials.

Inspection validates the request before it inspects the selected binding and never
applies it. A trainer binding must supply the complete version 4 catalog trainer
profile described in [TRAINING.md](TRAINING.md).
Include measured `requiredMiB`, GPU UUID, unit, executable and recipe paths and
digests, and private lifecycle paths. Inspection does not infer these values
from a service name or read the job envelope to discover them.

Inspection performs file and loaded-unit observations only. It does not execute
the trainer or preparation command, start or stop services, install dependencies,
write lifecycle files, change the catalog or enable a preset. All launch checks
run again at training start. The training UI remains separate in #456.
