# Deploy the supervisor

Requires Linux, host user-systemd, and unified cgroup v2 at `/sys/fs/cgroup`.
[Make a backup of state](OPERATIONS.md#back-up-before-an-upgrade) before changing binaries: even `status` or a proxy can migrate SQLite.

Use a [verified release](MAINTENANCE.md#source-and-binary-releases).

- [Configuration](#controller-configuration)
- [Unit stopping](#qualify-runtime-unit-stopping)
- [Workload release](#check-the-workload-release)
- [Migration](#migrate-release-checks)
- [Host qualification](#qualify-the-host)

## Controller configuration

The version 1 [catalog](INTEGRATION.md#configured-workloads) defines identities, health endpoints, release behavior and measured capacity requirements.
`configure` accepts it durably under the controller gate:

```sh
/PINNED/RELEASE/gpu-mode \
  -state /PRIVATE/DURABLE/STATE/state.db \
  -catalog /PRIVATE/catalog.json \
  configure
```

Other runtime commands pin accepted state. Without an accepted catalog, they fail before effects:

```sh
/PINNED/RELEASE/gpu-mode \
  -state /PRIVATE/DURABLE/STATE/state.db \
  -systemctl /ABSOLUTE/PATH/systemctl \
  status
```

Uppercase paths, units and `PORT` are placeholders.
Use absolute, checksummed, version-pinned binaries for the CLI and proxies, never mutable `PATH` selection.

Keep one explicit `-state` path in a durable service-owned directory with mode `0700`.
Manual commands and automation must share state, trusted executables, capacity settings and timeouts.

Put flags before the single command.

| Input | Meaning |
| --- | --- |
| `[runtime flags]` | Placeholder for `-state`, executable, capacity and timeout flags |
| `configure`, `verify-host` | Use `-catalog`, not accepted state |
| `restore-state` | Needs only `-state`. No runtime policy |
| `prune-audit` | Uses [audit flags](OPERATIONS.md#retain-or-archive-audit-history) |
| `-health-timeout` | Complete health check: opposing unit, cgroup probe and HTTP request |
| `-action-timeout` | Also bounds controller health and observation probes |
| `-verify-timeout` | Entire readiness phase, including polling. Earliest deadline wins |
| `-finalize-timeout` | Independent bounded finalization after verified readiness. Caller cancellation cannot strand the transition |

Before opening state or enabling automation, run `gpu-mode -catalog /PRIVATE/catalog.json [probe flags] verify-host`.
It checks hierarchy, manager anchor and unit mappings without state creation, locks, migration or unit changes.

Exit zero means capability success, not health or GPU-release proof. Errors return nonzero.
`-action-timeout` bounds it. Ctrl-C cancels it.

Lifecycle effects also start with capability preflight. It permits populated groups and removed cgroups of failed units. The release still requires stopped units and repeats checks after stopping.

Failed switch verification closes admission and requires recovery. Failed recovery preserves the error state.
For crashed units with unfinished work, stop proxies, then `resolve-work` and `recover`.

Recovery checks the stopped unit's empty cgroup before clearing retained systemd failure and rechecking `inactive/dead`.
Database conflicts or commit failures need inspection and recovery.

## Qualify runtime unit stopping

| Requirement | Action |
| --- | --- |
| Descendants | Keep all workers in the workload subtree. Set `KillMode` to `control-group` or qualified `mixed`, never `process` or `none` |
| Shutdown | Qualify the runtime's graceful mechanism and `KillSignal`. Forced termination after `TimeoutStopSec` must clear all workers |
| Budgets | Measure `TimeoutStopSec=STOP_BUDGET`. Set `-action-timeout ACTION_BUDGET` above the full systemd stop and `-cleanup-timeout CLEANUP_BUDGET` above all rollback actions and checks |
| Activation | Disable independent socket, path, timer, dependency and external-supervisor activation. Qualify `Restart`/`RestartSec` against controller ownership |

No universal GPU stop signal or safe timeout exists. Budget names are placeholders, not production recommendations from fixtures.
Test graceful and forced stops with the actual runtime, driver and workers. Unit completion or HTTP success cannot replace recursive cgroup evidence.

Owned launches keep `Restart=no`. Setup preview and explicit `apply` action add catalog-derived `Conflicts=` and acyclic `After=` between distinct owned units.
Existing owned units keep recorded bytes until Apply. Adopted units and drop-ins stay unchanged.

Manual opposing starts can interrupt work. Dependencies prove no authorization, admission, drain or GPU release. Arbitrary external units remain outside the guarantee.
Setup adds no custom `StartLimit*` policy. See [orchestrator decisions](INTEGRATION.md#orchestrator-decision).

<a id="verify-workload-release"></a>

## Check the workload release

The `systemd` adapter needs both units `inactive/dead` and descendant-inclusive `cgroup.events` `populated 0`. Removed workload cgroups also qualify.
Missing events in existing groups, unreadable or malformed evidence, metadata mismatch or surviving children fail closed.

The media UI is unavailable outside media mode.

<a id="select-an-explicit-media-policy"></a>

Every media profile must explicitly choose its `adapter`. Omission never authorizes stopping.
Choose `systemd` only after accepting UI shutdown and qualifying the stop.

Compatibility `media-unload` sends the configured release request and keeps the UI alive, but **live-media unload has no supported verifier**.
HTTP 2xx proves neither drain nor resource release. Verification fails closed promptly with an actionable error.

Use `systemd` or stop media before recovery.
Stopped media uses cgroup verification under either policy, including ownership changes and work resolution. Stopped text always needs verification.

Readiness and check-only recovery also check opposing release.
Text-to-media and idle-to-media switches prove text release without unloading the destination UI.

## Configure cgroups

Inspect the running units with `systemctl --user show --property=ControlGroup -- UNIT.service`.
Catalog paths must be canonical, absolute within `/sys/fs/cgroup`, non-root, distinct and non-overlapping. All workers must stay inside them.

Nonempty `ControlGroup` must match. After shutdown, an empty property uses the accepted catalog, never a PID or path cache. Blank metadata alone proves nothing.
Accept a new catalog if placement changes.

The same manager's loaded, active root `-.slice` must report an existing readable cgroup strictly containing both workload paths.
This anchor checks manager-to-mount mapping without privileged PID 1 access.

Missing or ambiguous anchors fail closed. The adapter rejects subtree or nested mounts and container mappings.

## Migrate release checks

Remove the rejected `-release-max-used-mib`, including `=0`. The release uses cgroups, never GPU memory accounting.
Move legacy unit, cgroup, health endpoint, adapter and capacity flags into catalog profiles. The v0.1.5 flags are not current migration commands.

Use `adapter: "systemd"` for service stops.

Stop the proxies and wait for shutdown and drain before `configure`. Setup and configuration enforce the proxy lifetime lock.
Updates need current `-configuration-revision`.

Commands pin durable acceptance, not edited files.
Restart proxies with updated configuration after acceptance.

Proxies pin their startup revision, reject affected new work with `configuration_changed`, and never hot-reload.
Keep execution closed until updated host and binding qualification.

<a id="check-capacity-before-startup"></a>

## Check the capacity before startup

Optional checks need `memory.free` to meet `requiredMiB` plus `-capacity-headroom-mib`, with `-nvidia-smi /ABSOLUTE/PATH/nvidia-smi` and `-gpu-index`.
`requiredMiB` is the measured target requirement. Zero or omission disables that target's check.

Driver cleanup delays or other users can cause capacity failure after successful release.
Repair capacity, then explicitly recover. Admission stays closed. A snapshot reserves no GPU memory.

## Validate configuration

Reject requirement-plus-headroom overflow, overlapping cgroups, shared profile units, unknown adapters and non-loopback health URLs.
`media-unload` requires a release URL. `systemd` does not, but any supplied URL still needs validation.

Obsolete flags fail even for `status` or `restore-state`.

`systemctl` and configured `nvidia-smi` must resolve to executable files owned by you or root.
Files, symlink targets and traversed ancestors must not allow world writes, except root-owned sticky ancestors such as `/tmp`, never the executable itself.

The single-admin policy trusts your group members and permits group writes only for groups you belong to.

<a id="trust-boundary"></a>

## Prevent bypasses

Lifecycle exclusion is not request fencing. Disable independent runtime startup and updates. Direct requests bypass admission and registered-work drain.
Supervised execution needs gates with no bypass during supervisor ownership. All commands sharing state pin the same accepted catalog.

Same-identity processes have full trust. Strong isolation needs [separate identities](../SECURITY.md).

## Qualify the host

Run `go test -race -count=1 -timeout=10m -tags=systemd_integration -run '^TestSystemd' -v ./internal/supervisor` with real user-systemd and cgroup v2 prerequisites.
Missing prerequisites fail, not skip.

| Evidence | Coverage and limits |
| --- | --- |
| Owned-backstop sleep fixtures | Generated dependencies, manual opposing starts, stop-before-start both ways, rapid switches, latched failures. Manual starts cannot resolve admitted work. No native GPU-driver release proof |
| Tagged suite | Isolated real units, kernel cgroups, SQLite. Lifecycle and ownership, restart and restore, descendants, drain, failed-unit preflight and work resolution and concurrency |
| Failure fixtures | Health endpoints. Deterministic health, release and inspection failures. Interrupted journal phases seeded before reopening |
| Deployment acceptance | Both switches and idle. User takeover, switch and return. Restart reconciliation and recovery. Worker release and delayed capacity. Backup restore |

CI does not qualify your GPU, driver, workload API or boot automation.
Confirm that direct access and independent activation cannot bypass the boundary.

Record the pinned-version and host evidence outside this public repository.
Service-stop mode removes the inactive media UI but adds no request fencing.

## Next

[Reconcile at boot](OPERATIONS.md#boot-and-explicit-recovery), configure the [proxy](INTEGRATION.md#configure-the-execution-proxy), and qualify its [adapter](INTEGRATION.md#contract-for-execution-proxy-adapters).
Keep execution closed until host and adapter qualification finish.
