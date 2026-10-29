# Orchestrator decision

The durable state machine, lease fencing, fail-closed admission and drain remain
our control authority. Runtime readiness, capacity and cgroup and GPU-release checks
remain required evidence. Unit ordering or completion cannot replace them.

Issue #397 adds systemd backstops to distinct supervisor-owned units in the
configured catalog. Setup derives `Conflicts=` for other owned units and `After=`
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

The hardened vLLM fields follow the same rule. A binding without them keeps
its exact earlier unit bytes and fingerprint, so setup can remove or replace
the unit. Such a binding never qualifies for execution. Unlike the
`owned.conflicts` upgrade, setup does not upgrade it. Configure the model again
or remove it, as [Hardened vLLM launch](WORKLOADS.md#hardened-vllm-launch)
describes.

Pending activations keep their original unit hashes through exact replay.
After recovery completes, setup offers the upgrade in a later preview. Runtime
startup never silently rewrites accepted files. This additive feature needs no database migration.
Each future schema change gets its own version with its feature.

Size limits:

| Data | Limit |
| --- | --- |
| Catalog canonical input, excluding derived owned conflicts | 256 KiB |
| Derived allowance for the supported 32-unit peer graph | 96 KiB |
| Full serialization on validation and readback | 352 KiB |
| Setup requests | 416 KiB |

Other fields cannot consume the derived allowance. The request limit reserves
64 KiB for the deployment profile and request envelope around a maximum catalog.

`requiredMiB` is a demand estimate, not an enforced memory ceiling.
Host evidence and hardware and build identifiers stay outside this decision record.
