# Orchestrator decision (#396)

The durable state machine, lease fencing, fail-closed admission and drain remain
our control authority. Runtime readiness, capacity and cgroup and GPU-release checks
remain required evidence. Unit ordering or completion cannot replace them.

Issue #397 adds systemd backstops to distinct supervisor-owned units in the
configured catalog. Shared Ollama profiles use one unit and never conflict with
that same unit. Setup derives `Conflicts=` for other owned units and `After=`
only for lexically earlier peers, giving each pair one ordering edge without
cycles. The [contract for systemd units](https://www.freedesktop.org/software/systemd/man/latest/systemd.unit.html#Conflicts=)
orders a conflicting stop before a start in either direction.
`Restart=no` remains. This change adds no `StartLimit*` policy.

These backstops cannot authorize a handoff or drain admitted work. Manually
starting an opposing owned unit can interrupt current work. Arbitrary external
units are outside this guarantee. Setup preserves adopted units and their drop-ins.

The optional `owned.conflicts` field pins dependencies into the existing owned
render, spec and fingerprint chain. As with optional owned executable paths, absent
metadata retains exact legacy version-2 unit bytes and fingerprints. Readers
that do not recognize the field reject it through strict JSON decoding. Setup
preview derives and displays replacement owned-unit writes. The explicit `apply` action
upgrades legacy units through the existing rollback and recovery journal.

Removing
a peer or replacing it with a separately qualified adopted binding
recomputes the remaining owned dependencies. An already guarded owned file does not qualify for adoption without changes.
Dependency directives remain outside the strict external launch grammar.
Setup does not strip them from external files.

Pending activations keep their original unit hashes through exact replay.
After recovery completes, setup offers the upgrade in a later preview. Runtime
startup never silently rewrites accepted files. This additive feature needs no database migration.
Each future schema change gets its own version with its feature.

Catalog canonical input has a 256 KiB limit, excluding derived owned conflicts. A
separate 96 KiB allowance accommodates the supported 32-unit peer graph, with
the full serialization bounded at 352 KiB on validation and readback. Other
fields cannot consume this derived allowance. Setup requests have a 416 KiB limit.
This reserves 64 KiB for the deployment profile and request envelope around a maximum catalog.

A native router remains additive, behind a spike and demonstrated parity for
readiness, drain, release and capacity. Keep existing control until that gate passes. `requiredMiB` is a demand estimate, not an enforced memory ceiling.
Defer llama-swap, Quadlet, HAMi, metrics and Litestream code until concrete demand and qualified experiments support them. Host evidence and hardware and build
identifiers stay outside this decision record.
