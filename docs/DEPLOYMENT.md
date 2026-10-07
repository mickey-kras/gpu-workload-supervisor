# Deploy the supervisor

[Documentation](README.md) | [Repository](../README.md)

Requires Linux with user-systemd and cgroup v2. Install a [verified release](RELEASING.md) and [back up existing state](RESTORING.md#back-up-before-an-upgrade) before changing binaries.

## Controller configuration

Runtime identities, health endpoints, release behavior, and measured capacity requirements are deployment configuration. They live in a version 1 workload [catalog](WORKLOADS.md), accepted durably while holding the controller gate:

```sh
/PINNED/RELEASE/gpu-mode \
  -state /PRIVATE/DURABLE/STATE/state.db \
  -catalog /PRIVATE/catalog.json \
  configure
```

Every other runtime command pins the accepted catalog from state and fails
before any effect when none has been accepted:

```sh
/PINNED/RELEASE/gpu-mode \
  -state /PRIVATE/DURABLE/STATE/state.db \
  -systemctl /ABSOLUTE/PATH/systemctl \
  status
```

The uppercase paths, unit names, and `PORT` values are placeholders. Rules:

- Select a checksummed release and use its absolute version-pinned binary paths
  for the CLI and every proxy; do not rely on a mutable `PATH` selection.
- Keep one explicit `-state` path in a durable directory owned by the service
  identity with mode `0700`.
- Use the same state path, trusted executables, capacity settings, and timeout
  flags in manual commands and automation.
- Put flags before the single command.
- Back up before changing binaries: opening state, including through `status`
  or a proxy, can apply database migrations.

In the examples below, `[runtime flags]` means `-state` plus the
trusted executable, capacity, and timeout flags; it is not a literal CLI
argument. `configure` and `verify-host` take `-catalog` instead of pinning
state. `restore-state` needs only `-state`; `prune-audit` uses the [audit maintenance flags](OPERATIONS.md#retain-or-archive-audit-history).

Timeout flags:

- `-health-timeout` bounds each complete health check, including the opposing
  unit/cgroup probe and HTTP request.
- `-action-timeout` also bounds controller health and observation probes.
- `-verify-timeout` bounds the entire readiness phase, including probes and
  polling; the earliest applicable deadline wins.

Failed switch verification closes admission and requires recovery, while failed
recovery keeps the existing error state. Before lifecycle effects, a capability
preflight checks the host hierarchy, manager anchor, and unit mappings while
allowing populated workload groups. Release checks still run after stopping to
detect changes.

Before opening state or enabling automation, run `gpu-mode -catalog
/PRIVATE/catalog.json [probe flags] verify-host`:

- It checks those capabilities against a candidate catalog without creating
  state, acquiring state locks, migrating SQLite, or starting/stopping units.
- Exit status zero means the capability check passed; errors return nonzero.
- `-action-timeout` bounds the probe and Ctrl-C cancels it.
- This is not GPU release or workload health proof.
- A failed unit may have a removed cgroup: preflight accepts that capability
  state, but release still requires stopped units.
- For crashed units with unfinished work, stop proxies and use the documented
  `resolve-work` then `recover` sequence.
- After stopping a failed unit, recovery verifies its cgroup is empty before
  clearing systemd's retained failure state and rechecking `inactive/dead`.

After target verification succeeds, durable finalization uses its own bounded
`-finalize-timeout`, so caller cancellation does not strand a verified transition.
Database conflicts or commit errors still require inspection and recovery.

## Qualify runtime unit stopping

- Keep every descendant in the workload subtree. Choose a unit `KillMode` that
  terminates the whole service (`control-group`, or qualified `mixed` behavior);
  `process` and `none` are unsuitable for descendant cleanup.
- Select the runtime's documented graceful stop mechanism and `KillSignal`, then
  verify that forced termination after `TimeoutStopSec` clears all workers. There
  is no universal GPU stop signal or safe timeout.
- Measure `TimeoutStopSec=STOP_BUDGET` against actual shutdown. Set controller
  `-action-timeout ACTION_BUDGET` above the complete systemd stop operation and
  `-cleanup-timeout CLEANUP_BUDGET` above all required rollback actions and checks.
  These are placeholders, not fixture-derived production recommendations.
- Disable independent activation (socket/path/timer/dependency triggers and
  external supervisors), and qualify `Restart`/`RestartSec` behavior so stopped
  workloads cannot restart outside the controller's ownership decision.

Test graceful and forced stopping with the real runtime, driver, and workers.
Unit completion or HTTP success never replaces recursive cgroup release evidence.
Profiles sharing one Ollama unit are the exception: the unit keeps running
across model switches, so their release evidence is the daemon's loaded-model
list, with cgroup emptiness required once the shared unit is dead.

## Verify workload release

The `systemd` catalog adapter verifies both units are `inactive/dead` and their
configured cgroup v2 subtrees have `cgroup.events` `populated 0`, which includes
descendants. Running units shared by sibling Ollama profiles are verified at
model level instead: any loaded model other than the sibling target's own fails
release.
A removed workload cgroup is also released; missing events in an existing group,
unreadable or malformed evidence, mismatched systemd metadata, and surviving
children fail closed. The UI is unavailable outside media mode.

- The compatibility `media-unload` adapter sends the configured release request
  and leaves the UI alive.
- **Live-media unload cannot currently be verified:** HTTP 2xx does not prove
  the runtime has drained work and released models/resources, and no supported
  runtime-specific verifier is implemented.
- Release therefore fails closed promptly with an actionable error.
- Use the `systemd` adapter, or explicitly stop media before recovery.
- Stopped media uses cgroup verification in either policy, including ownership
  changes and work resolution.
- Stopped text is always verified.
- Readiness and verify-only recovery also verify the opposing workload's release.
- Switching text or idle to media may retain the destination UI after text
  release is proven; it does not require unloading the destination itself.

## Configure cgroups

Configure every profile's cgroup path from deployment knowledge of the actual
systemd units
(e.g. inspect `systemctl --user show --property=ControlGroup -- UNIT.service` while
the unit is running). Paths are absolute **within** `/sys/fs/cgroup`, canonical,
non-root, distinct, and non-overlapping; only sibling profiles sharing one
Ollama unit repeat the same unit and cgroup. A nonempty systemd `ControlGroup` must
match; an empty property after shutdown uses the accepted catalog, never an
in-memory PID or path cache. Accept an updated catalog if unit placement changes.
Blank metadata alone is not evidence.
All workload workers must remain in their configured subtree. Use the host unified
cgroup v2 root mounted at `/sys/fs/cgroup`, alongside host user systemd. The same
manager's loaded, active root `-.slice` must report an existing, readable cgroup
that strictly contains both workload paths. This anchor validates manager-to-mount
mapping without privileged access to PID 1. Subtree/nested mounts and container
mappings are unsupported; missing or ambiguous manager anchors fail closed.

## Select an explicit media policy

Every media profile selects its stop policy explicitly through its catalog
`adapter`: `systemd` stops the unit, while the compatibility `media-unload`
adapter sends the release request. Choose `systemd` only after accepting that it
stops the media UI and qualifying its unit shutdown. No omitted field silently
authorizes a service stop. `restore-state` requires no runtime policy.

## Migrate release checks

Remove `-release-max-used-mib` (including explicit `=0`; it is rejected) and
configure cgroup paths in the catalog. Release uses cgroup evidence, never GPU
memory accounting (running shared Ollama units use the loaded-model list, as
above). See the [v0.1.5 operator notes](releases/v0.1.5.md) for the
full migration.

## Check capacity before startup

Optional pre-start capacity checks use a profile's `requiredMiB` (measured target
requirement) plus `-capacity-headroom-mib`. A zero or omitted target requirement
disables that target's check.
Configure `-nvidia-smi /ABSOLUTE/PATH/nvidia-smi` and `-gpu-index` when using these
checks. Available `memory.free` must meet requirement plus headroom; delayed driver
cleanup or other users can cause a distinct capacity error after successful
release. Capacity failures keep admission closed and require explicit recovery;
repair capacity before retrying. A capacity snapshot cannot reserve GPU memory.

## Validate configuration

Invalid configuration is rejected:

- A profile requirement plus headroom must not overflow.
- Workload cgroups must not overlap.
- One unit must not serve two profiles.
- The release URL is required under `media-unload`; the `systemd` adapter does
  not need one, but a supplied URL is still validated.
- An unknown adapter is invalid.
- Every health URL must be a loopback URL.
- The `systemctl` executable, and `nvidia-smi` when configured, must resolve to
  root-owned executable files under root-owned directories, with no group/world
  writable component.
- Supplying an obsolete flag is an error even for `status` or `restore-state`.

## Prevent bypasses

This policy provides service lifecycle exclusion, not request fencing. Disable
independent runtime startup and updates in deployment configuration. Direct
runtime requests bypass admission and registered-work draining; execution gates
are still required for supervised requests. Every command sharing a state store
pins the same durably accepted catalog.

## Qualify the host

The tagged suite (`go test -race -count=1 -timeout=10m -tags=systemd_integration
-run '^TestSystemd' -v ./internal/supervisor`) uses isolated real user-systemd
units, kernel cgroup v2 evidence, and SQLite state. It covers lifecycle and
ownership transitions, restart/restore, descendant release, admission draining,
failed-unit preflight and work resolution, and concurrent commands. Health
endpoints are fixtures; health, release, and
inspection failures are injected deterministically, and interrupted journal phases
are seeded before reopening state. Selecting the tag requires real systemd/cgroup
prerequisites and fails rather than silently skipping when they are unavailable.
These tests do not qualify an operator's GPU, driver, workload API, or boot
automation. Passing CI is not evidence that a particular host has passed acceptance.

Before enabling execution on a deployment, verify both switch directions and idle,
user takeover/switch/return, restart reconciliation, explicit failure recovery,
worker subtree release, delayed capacity availability, and backup restoration.
Confirm that direct runtime access and independent activation cannot bypass the
intended boundary. Record the pinned version and deployment-specific evidence
outside this public repository. Service-stop mode removes the inactive media UI
and does not itself provide request fencing.

## Trust boundary

Processes sharing the supervisor identity are fully trusted; strong isolation requires separate service identities (see [SECURITY.md](../SECURITY.md)). Execution requests must not have a route that bypasses the gate while supervisor ownership is active.

## Next

[Reconcile at boot](OPERATIONS.md#boot-and-explicit-recovery), configure the [execution proxy](EXECUTION-PROXY.md), and verify its [adapter contract](PROXY-CONTRACT.md). Keep execution closed until both host and adapter qualification are complete.
