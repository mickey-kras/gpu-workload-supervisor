# Deploy the supervisor

[Documentation](README.md) | [Repository](../README.md)

Runtime identities, health endpoints, release behavior, and measured capacity requirements are deployment configuration:

```sh
/PINNED/RELEASE/gpu-mode \
  -state /PRIVATE/DURABLE/STATE/state.db \
  -text-unit TEXT.service \
  -media-unit MEDIA.service \
  -text-health-url http://127.0.0.1:PORT/health \
  -media-health-url http://127.0.0.1:PORT/health \
  -media-stop-mode stop-service \
  -text-cgroup /DEPLOYMENT/CGROUP/TEXT.service \
  -media-cgroup /DEPLOYMENT/CGROUP/MEDIA.service \
  -systemctl /ABSOLUTE/PATH/systemctl \
  status
```

The uppercase paths, unit names, and `PORT` values are placeholders. Select a
checksummed release and use its absolute version-pinned binary paths for the CLI
and every proxy; do not rely on a mutable `PATH` selection. Keep one explicit
`-state` path in a durable directory owned by the service identity with mode
`0700`. Use the same state path, units, endpoints, cgroups, stop policy, capacity
settings, trusted executables, and timeout flags in manual commands and automation.
Put flags before the single command. In the examples below, `[runtime flags]`
means this complete deployment configuration, including `-state`; it is not a
literal CLI argument. `restore-state` is the exception and needs only `-state`.
Opening state, including through `status` or a proxy, can apply database migrations;
back up before changing binaries.

`-health-timeout` bounds each complete health check, including the opposing
unit/cgroup probe and HTTP request. `-action-timeout` also bounds controller health
and observation probes. `-verify-timeout` bounds the entire readiness phase,
including probes and polling; the earliest applicable deadline wins. Failed
switch verification closes admission and requires recovery, while failed recovery
keeps the existing error state.

## Verify workload release

`stop-service` verifies both units are `inactive/dead` and their configured
cgroup v2 subtrees have `cgroup.events` `populated 0`, which includes descendants.
A removed workload cgroup is also released; missing events in an existing group,
unreadable or malformed evidence, mismatched systemd metadata, and surviving
children fail closed. The UI is unavailable outside media mode.

The default `-media-stop-mode unload` still sends the configured release request
and leaves the UI alive. **Live-media unload cannot currently be verified:** HTTP
2xx does not prove the runtime has drained work and released models/resources, and
no supported runtime-specific verifier is implemented. Release therefore fails
closed with an actionable error. Use `stop-service`, or explicitly stop media
before recovery. Stopped media uses cgroup verification in either policy, including
ownership changes and work resolution. Stopped text is always verified. Readiness
and verify-only recovery also verify the opposing workload's release. Switching
text or idle to media may retain the destination UI after text release is proven;
it does not require unloading the destination itself.

## Configure cgroups

Configure both cgroup paths from deployment knowledge of the actual systemd units
(e.g. inspect `systemctl --user show --property=ControlGroup -- UNIT.service` while
the unit is running). Paths are absolute **within** `/sys/fs/cgroup`, canonical,
non-root, distinct, and non-overlapping. A nonempty systemd `ControlGroup` must
match; an empty property after shutdown uses the explicit configuration, never an
in-memory PID or path cache. Keep configuration consistent across CLI invocations
and update it if unit placement changes. Blank metadata alone is not evidence.
All workload workers must remain in their configured subtree. Use the host unified
cgroup v2 root mounted at `/sys/fs/cgroup`, alongside host user systemd. The same
manager's loaded, active root `-.slice` must report an existing, readable cgroup
that strictly contains both workload paths. This anchor validates manager-to-mount
mapping without privileged access to PID 1. Subtree/nested mounts and container
mappings are unsupported; missing or ambiguous manager anchors fail closed.

## Migrate release checks

**Migration:** remove `-release-max-used-mib` and configure both cgroup paths.
Every explicit use of the old flag, including `=0`, is rejected before opening
state. Do not replace it with a larger threshold or a learned idle baseline.
Release never queries total GPU memory or GPU process accounting: unrelated desktop
allocations, PID reuse, and unavailable accounting cannot change the result.
Cgroup evidence proves workload processes are gone, not that asynchronous driver
cleanup has finished.

## Check capacity before startup

Optional pre-start capacity checks use `-text-required-mib` and/or
`-media-required-mib` (measured target requirements) plus
`-capacity-headroom-mib`. A zero target requirement disables that target's check.
Configure `-nvidia-smi /ABSOLUTE/PATH/nvidia-smi` and `-gpu-index` when using these
checks. Available `memory.free` must meet requirement plus headroom; delayed driver
cleanup or other users can cause a distinct capacity error after successful
release. Capacity failures keep admission closed and require explicit recovery;
repair capacity before retrying. A capacity snapshot cannot reserve GPU memory.

## Validate configuration

Invalid configuration is rejected: do not set headroom without at least one
nonzero target requirement, overlap the workload cgroups, reuse one unit for both
workloads, or omit the release URL under `unload`. `stop-service` does not need a
release URL; if supplied, it is still validated. An unknown or empty stop policy
is invalid. Both health URLs must be loopback URLs. The `systemctl` executable,
and `nvidia-smi` when configured, must resolve to root-owned executable files under
root-owned directories, with no group/world writable component. Supplying an
obsolete flag is an error even for `status` or `restore-state`.

## Prevent bypasses

This policy provides service lifecycle exclusion, not request fencing. Disable
independent runtime startup and updates in deployment configuration. Direct
runtime requests bypass admission and registered-work draining; execution gates
are still required for supervised requests. Use the same stop policy for every
command sharing a state store.

The state directory must be private and owned by the current user. Commands use an exclusive file lock. Opening the store applies pending migrations. Boot reconciliation does not resume incomplete work. Interrupted transitions and latched errors require explicit recovery.



## Qualify the host

The tagged suite (`go test -race -count=1 -timeout=10m -tags=systemd_integration
-run '^TestSystemd' -v ./internal/supervisor`) uses isolated real user-systemd
units, kernel cgroup v2 evidence, and SQLite state. It covers lifecycle and
ownership transitions, restart/restore, descendant release, admission draining,
and concurrent commands. Health endpoints are fixtures; health, release, and
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

Processes sharing the supervisor identity are trusted. A compromised process with the same operating-system permissions can bypass advisory locks, state files, and service control. Strong isolation requires separate service identities and operating-system enforced permissions.

Execution requests must not have a route that bypasses the gate while supervisor ownership is active.
