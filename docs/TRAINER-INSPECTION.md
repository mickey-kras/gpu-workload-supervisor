# Trainer binding inspection

`gpu-setup discover` adds a separate `trainers` array for explicit trainer
profiles in the current catalog or pending activation. Existing application
discovery does not change. An empty array means no explicit trainer bindings were
available, not that the host lacks training software.

Each result identifies the workload and unit, reports `bindingStatus`, and keeps
`qualificationStatus: "unqualified"`. A `verified` binding means the same
executable, recipe, exact unit source, loaded command and environment, and
physical GPU identity checks used before training passed. It does not approve
the Python environment, prove measured capacity, test GPU kernels or authorize
launch. Failures stay visible with generic diagnostic guidance, without
subprocess output or lifecycle credentials.

Inspection validates the request before inspecting the selected binding and
never applies it. A trainer binding must supply the complete catalog trainer
profile (catalog version 4 or 5) in [TRAINING.md](TRAINING.md). This includes
measured `requiredMiB`, GPU UUID, unit, executable and recipe paths and digests,
and private lifecycle paths.
Inspection infers none of these from a service name or the job envelope.
A profile with resource limits elsewhere in the catalog does not block the
inspection.

Inspection performs file and loaded-unit observations only. It does not execute
the trainer or preparation command, start or stop services, install
dependencies, write lifecycle files, change the catalog or enable a preset. All
launch checks run again at training start. The training UI remains separate in
#456.
