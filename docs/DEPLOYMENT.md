# Deploy the supervisor

Linux with user-systemd and cgroup v2 is necessary.
Before you change binaries, [make a backup of existing state](OPERATIONS.md#back-up-before-an-upgrade).
Install a [verified release](MAINTENANCE.md#source-and-binary-releases).

## Controller configuration

Runtime identities, health endpoints, release behavior, and measured capacity requirements are deployment configuration. They live in a version 1 workload [catalog](INTEGRATION.md#configured-workloads), accepted durably while holding the controller gate:

```sh
/PINNED/RELEASE/gpu-mode \
  -state /PRIVATE/DURABLE/STATE/state.db \
  -catalog /PRIVATE/catalog.json \
  configure
```

Every other runtime command pins the accepted catalog from state and fails
before any effect if state contains no accepted catalog:

```sh
/PINNED/RELEASE/gpu-mode \
  -state /PRIVATE/DURABLE/STATE/state.db \
  -systemctl /ABSOLUTE/PATH/systemctl \
  status
```

Uppercase paths, unit names, and `PORT` values are placeholders. Rules:

- Use the absolute version-pinned binary paths of a checksummed release for the
  CLI and every proxy. Never rely on a mutable `PATH` selection.
- Keep one explicit `-state` path in a durable directory owned by the service
  identity with mode `0700`.
- Use the same state path, trusted executables, capacity settings, and timeout
  flags in manual commands and automation.
- Put flags before the single command.
- Make a backup before changing binaries: opening state, including through
  `status` or a proxy, can apply database migrations.

Below, `[runtime flags]` means `-state` plus the trusted executable, capacity,
and timeout flags. It is not a literal CLI argument. `configure` and
`verify-host` take `-catalog` instead of pinning state. `restore-state` needs
only `-state`. `prune-audit` uses the [audit maintenance flags](OPERATIONS.md#retain-or-archive-audit-history).

Timeout flags:

- `-health-timeout` bounds each complete health check, including the opposing
  unit and cgroup probe and HTTP request.
- `-action-timeout` also bounds controller health and observation probes.
- `-verify-timeout` bounds the entire readiness phase, including probes and
  polling. The earliest applicable deadline wins.

Failed switch verification closes admission and requires recovery. Failed
recovery keeps the existing error state. Before lifecycle effects, a capability
preflight checks the host hierarchy, manager anchor, and unit mappings while
allowing populated workload groups. The release checks still run after stopping
to detect changes.

Before opening state or enabling automation, run `gpu-mode -catalog
/PRIVATE/catalog.json [probe flags] verify-host`:

- It checks those capabilities against a candidate catalog without creating
  state, acquiring state locks, migrating SQLite, or starting or stopping units.
- Exit status zero means the capability check passed. Errors return nonzero.
- `-action-timeout` bounds the probe. Ctrl-C cancels it.
- It is not GPU release or workload health proof.
- A failed unit can have a removed cgroup: preflight accepts that capability
  state, but release still requires stopped units.
- For crashed units with unfinished work, stop proxies and use the documented
  `resolve-work` then `recover` sequence.
- After stopping a failed unit, recovery verifies its cgroup is empty before
  clearing systemd's retained failure state and rechecking `inactive/dead`.

After target verification succeeds, durable finalization uses its own bounded
`-finalize-timeout`, so caller cancellation cannot strand a verified transition.
Database conflicts or commit errors still need inspection and recovery.

## Qualify runtime unit stopping

- Keep every descendant in the workload subtree. Choose a unit `KillMode` that
  stops the whole service (`control-group`, or qualified `mixed` behavior).
  `process` and `none` cannot clean up descendants.
- Select the runtime's documented graceful stop mechanism and `KillSignal`,
  then check that forced termination after `TimeoutStopSec` clears all workers.
  There is no universal GPU stop signal or safe timeout.
- Measure `TimeoutStopSec=STOP_BUDGET` against actual shutdown. Set the controller
  `-action-timeout ACTION_BUDGET` above the full systemd stop operation and
  `-cleanup-timeout CLEANUP_BUDGET` above all required rollback actions and checks.
  These are placeholders, not fixture-derived production recommendations.
- Disable independent activation (socket, path, timer and dependency triggers and
  external supervisors). Qualify `Restart`/`RestartSec` behavior so stopped
  workloads cannot restart outside the controller's ownership decision.

Test graceful and forced stopping with the real runtime, driver, and workers.
Unit completion or HTTP success never replaces recursive cgroup release evidence.

Supervisor-owned launches keep `Restart=no`. Setup preview and the explicit
`apply` action add catalog-derived `Conflicts=` and acyclic `After=` between
distinct owned units. Existing owned catalogs keep their recorded unit bytes
until Apply. Setup preserves adopted units and drop-ins.

A manual opposing start can interrupt work. These dependencies give no
authorization, admission, drain or GPU-release proof. Arbitrary external units
remain outside the guarantee. Setup adds no custom `StartLimit*` policy. See
[orchestrator decisions](INTEGRATION.md#orchestrator-decision).

<a id="verify-workload-release"></a>

## Check the workload release

The `systemd` catalog adapter verifies both units are `inactive/dead` and their
configured cgroup v2 subtrees have `cgroup.events` `populated 0`, which includes
descendants. It also treats a removed workload cgroup as released. Missing
events in an existing group, unreadable or malformed evidence, mismatched
systemd metadata, and surviving children fail closed. The UI is unavailable
outside media mode.

- The compatibility `media-unload` adapter sends the configured release request
  and leaves the UI alive.
- **The supervisor cannot currently check live-media unload.** HTTP 2xx does not
  prove drained work or released models and resources. No supported
  runtime-specific verifier exists.
- The release check therefore fails closed promptly with an actionable error.
- Use the `systemd` adapter, or explicitly stop media before recovery.
- Stopped media uses cgroup verification in either policy, including ownership
  changes and work resolution.
- The adapter always verifies stopped text.
- Readiness and check-only recovery additionally check the opposing workload's release.
- Switching text or idle to media can keep the destination UI after the
  supervisor proves text release. The switch does not unload the destination
  itself.

## Configure cgroups

Configure every profile's cgroup path from deployment knowledge of the actual
systemd units. For example, inspect
`systemctl --user show --property=ControlGroup -- UNIT.service` while the unit
runs. Paths are
absolute **within** `/sys/fs/cgroup`, canonical, non-root, distinct, and
non-overlapping. A nonempty systemd `ControlGroup` must match.

An empty property after shutdown uses the accepted catalog, never an in-memory
PID or path cache. Accept an updated catalog if unit placement changes. Blank
metadata alone is not evidence. All workload workers must remain in their
configured subtree.

Use the host unified cgroup v2 root mounted at `/sys/fs/cgroup`, alongside host
user systemd. The same manager's loaded, active root `-.slice` must report an
existing, readable cgroup that strictly contains both workload paths. This
anchor validates manager-to-mount mapping without privileged access to PID 1.
The adapter does not support subtree or nested mounts or container mappings.

Missing or ambiguous manager anchors fail closed.

## Select an explicit media policy

Every media profile selects its stop policy explicitly through its catalog
`adapter`: `systemd` stops the unit. The compatibility `media-unload` adapter
sends the release request. Choose `systemd` only after accepting that it stops
the media UI and qualifying its unit shutdown. No omitted field silently
authorizes a service stop. `restore-state` requires no runtime policy.

## Migrate release checks

Remove `-release-max-used-mib`, including explicit `=0`, which the CLI rejects.
Configure cgroup paths in the catalog: the release check uses cgroup evidence,
never GPU memory accounting. Move the legacy runtime flags for each workload to
profiles in the accepted [catalog](INTEGRATION.md#configured-workloads), including unit, cgroup,
health endpoint, adapter and capacity requirement.

Use `adapter: "systemd"` for service-stop behavior. Accept the catalog with
`configure`. Updates need the current `-configuration-revision`. Runtime
commands pin that durable catalog rather than reading edited files.

The per-workload flags from v0.1.5 are not current migration commands.

Stop the workload proxies and wait for their shutdown and drain to finish
before changing the accepted catalog. Setup and `configure` enforce the proxy
lifetime lock. After acceptance, restart each proxy with the updated deployment
configuration. A proxy pins the catalog revision at startup and rejects
affected new work with `configuration_changed` if that revision changes. It
does not hot-reload the catalog. Keep execution closed until you qualify the
updated bindings and host.

<a id="check-capacity-before-startup"></a>

## Check the capacity before startup

Optional pre-start capacity checks use a profile's `requiredMiB` (measured
target requirement) plus `-capacity-headroom-mib`. A zero or omitted target
requirement disables that target's check. Configure
`-nvidia-smi /ABSOLUTE/PATH/nvidia-smi` and `-gpu-index` when using these
checks. Available `memory.free` must meet requirement plus headroom.

Delayed driver cleanup or other users can cause a distinct capacity error after
successful release. Capacity failures keep admission closed and need explicit
recovery. Repair capacity before retrying. A capacity snapshot does not reserve
memory on the GPU.

## Validate configuration

The validator rejects invalid configuration:

- A profile requirement plus headroom must not overflow.
- Workload cgroups must not overlap.
- One unit must not serve two profiles (see [configured workloads](INTEGRATION.md#configured-workloads)).
- The `media-unload` adapter needs a release URL. The `systemd` adapter does
  not, but the validator still checks a supplied URL.
- An unknown adapter is invalid.
- Every health URL must be a loopback URL.
- The `systemctl` executable, and `nvidia-smi` when configured, must resolve to
  executable files that you or root own. No file or directory part can allow
  world write access, except root-owned sticky ancestor directories such as
  `/tmp`. This exception does not apply to the executable itself. The validator
  checks symlink targets and traversed ancestors. The single-admin host policy
  trusts members of your own groups and permits group write only for groups you
  belong to.
- Supplying an obsolete flag is an error even for `status` or `restore-state`.

## Prevent bypasses

This policy gives service lifecycle exclusion, not request fencing. Disable
independent runtime startup and updates in deployment configuration. Direct
runtime requests bypass admission and registered-work draining. Supervised
requests still need execution gates. Every command sharing a state store pins
the same durably accepted catalog.

## Qualify the host

The owned-backstop cases use generated dependencies with disposable sleep
services. They check manual opposing starts and completed-stop-before-start
ordering in both directions. They also check rapid controller switching,
latched failures, and that manual starts do not resolve durable admitted work.
They do not prove GPU-driver release for a native runtime.

The tagged suite (`go test -race -count=1 -timeout=10m -tags=systemd_integration
-run '^TestSystemd' -v ./internal/supervisor`) uses isolated real user-systemd
units, kernel cgroup v2 evidence, and SQLite state. It covers lifecycle and
ownership transitions, restart and restore, descendant release, admission
draining, failed-unit preflight and work resolution, and concurrent commands.
Health endpoints are fixtures. The tests inject health, release, and inspection
failures deterministically.

They seed interrupted journal phases before reopening state. Selecting the tag
needs real systemd and cgroup prerequisites and fails rather than silently
skipping when they are unavailable. These tests do not qualify an operator's
GPU, driver, workload API, or boot automation. Passing CI is not evidence that
a particular host passed acceptance.

Before you enable execution on a deployment, check these operations:

- Both switch directions and idle.
- User takeover, switch and return.
- Restart reconciliation and explicit failure recovery.
- Worker subtree release and delayed capacity availability.
- Backup restoration.

Confirm that direct runtime access and independent activation cannot bypass the
intended boundary. Record the pinned version and deployment-specific evidence
outside this public repository. Service-stop mode removes the inactive media UI
and does not itself give request fencing.

## Trust boundary

The supervisor fully trusts processes that share its identity. Strong isolation
requires separate service identities (see [SECURITY.md](../SECURITY.md)).
Execution requests must have no route that bypasses the gate while supervisor
ownership is active.

## Next

[Reconcile at boot](OPERATIONS.md#boot-and-explicit-recovery), configure the [execution proxy](INTEGRATION.md#configure-the-execution-proxy), and check its [adapter contract](INTEGRATION.md#contract-for-execution-proxy-adapters). Keep execution closed until both host and adapter qualification are complete.
