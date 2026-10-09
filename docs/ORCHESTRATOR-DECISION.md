# Orchestrator decision (#396)

The durable state machine, lease fencing, fail-closed admission and drain remain
our control authority. Runtime readiness, capacity and cgroup/GPU-release checks
remain required evidence. Unit ordering or completion cannot replace them.

Issue #397 adds systemd backstops to distinct supervisor-owned units in the
configured catalog. Shared Ollama profiles use one unit and never conflict with
that same unit. Setup derives `Conflicts=` for other owned units and `After=`
only for lexically earlier peers, giving each pair one ordering edge without
cycles. The [systemd unit contract](https://www.freedesktop.org/software/systemd/man/latest/systemd.unit.html#Conflicts=)
orders a conflicting stop before a start in either direction.
`Restart=no` remains; no `StartLimit*` policy is introduced.

These backstops cannot authorize a handoff or drain admitted work. Manually
starting an opposing owned unit can interrupt current work. Arbitrary external
units are outside this guarantee; adopted units and their drop-ins are preserved.

The optional `owned.conflicts` field pins dependencies into the existing owned
render/spec/fingerprint chain. As with optional owned executable paths, absent
metadata retains exact legacy version-2 unit bytes and fingerprints. Readers
that do not recognize the field reject it through strict JSON decoding. Setup
preview derives and displays replacement owned-unit writes; explicit Apply
upgrades legacy units through the existing rollback/recovery journal. Removing
a peer or replacing it with a separately qualified adopted binding
recomputes the remaining owned dependencies. An already guarded owned file does
not qualify as adopted unchanged: dependency directives remain outside the strict
external launch grammar, and setup does not strip them from external files.
Pending activations retain their original unit hashes through exact replay;
the upgrade is offered in a later preview after recovery completes. Runtime
startup never silently rewrites accepted files. This additive feature needs no
database
migration; future schema changes are versioned with their own feature. Catalog
serialization is bounded at 256 KiB on validation and readback, accommodating
the supported 32-unit peer graph within the setup request size boundary.

A native router remains additive, behind a spike and demonstrated parity for
readiness, drain, release and capacity. Existing control is retained until that
gate passes. `requiredMiB` is a demand estimate, not an enforced memory ceiling.
llama-swap, Quadlet, HAMi, metrics and Litestream code are deferred until concrete
demand and qualified experiments support them. Host evidence and hardware/build
identifiers stay outside this decision record.
