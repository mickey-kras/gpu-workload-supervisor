# Host-protection resource limits (#584)

[Documentation](README.md) | [Repository](../README.md)

This record gives the design for
[#584](https://github.com/mickey-kras/gpu-workload-supervisor/issues/584).
The current release does not have this feature. It renders no resource-control
directives. `requiredMiB` stays a VRAM demand estimate before launch. It is not
an enforced ceiling. GPU VRAM enforcement is out of scope.

## Method

Statements marked "from systemd sources" come from the source files at tags
`v252`, `v255` and `v259`. The links give the exact lines. Kernel statements come
from the [cgroup v2 guide](https://docs.kernel.org/admin-guide/cgroup-v2.html).
The kernel guide has no version pin, so the qualification on each host kernel
must confirm those statements. No running user manager was available, so the
spike started no probe unit. The [qualification procedure](#qualification-procedure)
gives the host checks that must confirm each claim before an operator enables
limits.

This record keeps one offline parse check. It passes valid and invalid values the
same way. With systemd 255, `systemd-analyze verify` printed no warning and
exited with status 0 for the unit below. The same command with `MemoryMax=bogus`
also exited with status 0 and printed only a warning. Thus the renderer must
emit only validated values, and the loaded-property check must confirm each
value.

```sh
systemd-analyze verify ./gws-limits-probe.service
```

```ini
[Unit]
Description=Resource limit directive probe
[Service]
Type=exec
Restart=no
OOMPolicy=stop
MemoryHigh=49152M
MemoryMax=57344M
MemorySwapMax=0
TasksMax=4096
CPUWeight=100
CPUQuota=800%
IOWeight=100
IOReadBandwidthMax=/dev/nvme0n1 524288000
IOWriteBandwidthMax=/dev/nvme0n1 262144000
IOReadIOPSMax=/dev/nvme0n1 20000
IOWriteIOPSMax=/dev/nvme0n1 10000
ExecStart=/bin/true
```

## Trust model

There are two layers. They protect against different failures.

| Layer | Owner | Guarantee | Outside the guarantee |
| --- | --- | --- | --- |
| Host cap on `user-UID.slice` | Root | The cap bounds every process inside the slice, also hostile code | Each other way for the service UID to start processes outside the slice. Examples are `cron`, `at`, the `docker`, `podman` and `libvirt` groups, `sudo` or `polkit` grants for a system manager, and other system services. Workloads inside the slice share the cap with no isolation from each other |
| Unit limits on each owned unit | Service UID | Protection against runaway workloads | Hostile code at the service UID |

Code that runs as the service UID can change the limits of its own owned units.
It can stop the supervisor, edit the unit files and reload the user manager. The
[security policy](../SECURITY.md) already treats the service UID as trusted.
Thus unit limits are protection against accidents, not a security boundary.
Isolation between workloads with separate service accounts is out of scope.

The slice contains `user@UID.service` and the session scopes of the UID, for
example from `sshd` or a display manager
([v252](https://github.com/systemd/systemd/blob/v252/man/user%40.service.xml#L59),
[v255](https://github.com/systemd/systemd/blob/v255/man/user%40.service.xml#L59),
[v259](https://github.com/systemd/systemd/blob/v259/man/user%40.service.xml#L60)).
From systemd sources, delegation makes only `cgroup.procs`,
`cgroup.subtree_control` and `cgroup.threads` writable by the user. At `v259`,
`memory.oom.group` and `memory.reclaim` are also writable
([v252](https://github.com/systemd/systemd/blob/v252/src/shared/cgroup-setup.c#L399),
[v255](https://github.com/systemd/systemd/blob/v255/src/shared/cgroup-setup.c#L400),
[v259](https://github.com/systemd/systemd/blob/v259/src/shared/cgroup-setup.c#L194)).
systemd does not delegate the slice, so root owns all of its limit files. The
kernel guide states that a delegatee cannot escape the limits of its parent.

Setup does not try to prove that no escape exists. It checks the common routes
that it can see:

| Route | Check | Result |
| --- | --- | --- |
| `cron` | Daemon or binary absent: pass. `cron.allow` present without the user: pass. Only `cron.deny` present and it lists the user: pass. Any other case: fail | Fail closed. The error names the file to create or edit |
| `at` | The same rule with `at.allow` and `at.deny` | Fail closed. The error names the file to create or edit |
| Groups | Membership in `docker`, `podman`, `libvirt`, `lxd`, `incus`, `sudo`, `wheel` or `disk` | Warning that names each group |
| `sudo` | Rules for the user that setup can read with `sudo -l -U` | Warning that names each rule |
| System units | A loaded system unit with `User=` set to the service UID | Warning that names each unit |

## Host cap

The host cap is one manual root step with two system drop-ins. An example for
UID 1000 follows. The first drop-in caps the slice. The second drop-in delegates
`io` to the user manager.

```ini
# /etc/systemd/system/user-1000.slice.d/50-gws-host-cap.conf
[Slice]
MemoryHigh=57344M
MemoryMax=61440M
MemorySwapMax=0
TasksMax=16384
CPUQuota=1400%
IOReadBandwidthMax=/dev/nvme0n1 1073741824
IOWriteBandwidthMax=/dev/nvme0n1 536870912
```

```ini
# /etc/systemd/system/user@1000.service.d/50-gws-delegate-io.conf
[Service]
Delegate=io
```

The `user@.service` manual names drop-ins for `user-UID.slice` as the place for
the limits of one user
([v252](https://github.com/systemd/systemd/blob/v252/man/user%40.service.xml#L80),
[v255](https://github.com/systemd/systemd/blob/v255/man/user%40.service.xml#L80),
[v259](https://github.com/systemd/systemd/blob/v259/man/user%40.service.xml#L81)).
From systemd sources, a controller list in `Delegate=` adds to the earlier list
([v252](https://github.com/systemd/systemd/blob/v252/src/core/load-fragment.c#L3974),
[v255](https://github.com/systemd/systemd/blob/v255/src/core/load-fragment.c#L4018),
[v259](https://github.com/systemd/systemd/blob/v259/src/core/load-fragment.c#L3991)).
Thus `Delegate=io` keeps the default `pids memory cpu`.

To install or change the drop-ins, the operator first stops all work. The
supervisor must report verified idle release, and external service management
stays paused. Then the administrator runs `systemctl daemon-reload`.

The slice limits apply at the reload. Only a new `Delegate=` value needs a
restart of `user@UID.service`. That restart stops the supervisor and every owned
unit. If a restart occurs without idle release, the existing
[boot reconciliation](OPERATIONS.md#boot-and-explicit-recovery) rules apply.

### Known drop-ins

Upstream systemd ships vendor drop-ins for both units at all three versions.
`user-.slice.d/10-defaults.conf` sets `TasksMax=33%` and `StopWhenUnneeded=yes`
([v252](https://github.com/systemd/systemd/blob/v252/units/user-.slice.d/10-defaults.conf),
[v255](https://github.com/systemd/systemd/blob/v255/units/user-.slice.d/10-defaults.conf),
[v259](https://github.com/systemd/systemd/blob/v259/units/user-.slice.d/10-defaults.conf)).
`user@.service.d/10-login-barrier.conf` adds an ordering dependency
([v252](https://github.com/systemd/systemd/blob/v252/units/user%40.service.d/10-login-barrier.conf),
[v255](https://github.com/systemd/systemd/blob/v255/units/user%40.service.d/10-login-barrier.conf),
[v259](https://github.com/systemd/systemd/blob/v259/units/user%40.service.d/10-login-barrier.conf)).
Distributions can add more files in the same directories.

The known set for each unit has these members:

1. The two drop-ins of the host cap, with the exact names above.
2. Vendor drop-ins under `/usr/lib/systemd/system` or `/lib/systemd/system`.
   The package manager must report each one as a file of the systemd package
   with no local change. Root owns the file and every parent directory, with no
   group or other write.

Each other file in `DropInPaths` is unknown. An unknown file fails closed, and
the error names its path. The drop-in names start with `50-`, so they sort
after `10-defaults.conf`. systemd reads drop-ins in name order, so the
`TasksMax` of the host cap replaces the vendor `TasksMax=33%`. The `systemd.unit`
manual defines this name order. The checks of the
effective properties and cgroup files below decide the result, whatever drop-ins
exist. Unit tests use a fixture with the vendor drop-ins and a fake package
manager.

### Host cap checks

Every unit with limits needs the host cap. Setup shows the exact drop-ins with
the selected values. Setup and runtime preflight check these items before setup
accepts a unit with limits and before each launch of one:

1. Each drop-in of the host cap is a regular file, not a symlink. Root owns the
   file and every parent directory up to `/etc`. None of them has group or
   other write.
2. `systemctl show` for the slice and for `user@UID.service` lists only known
   drop-ins in `DropInPaths`. It reports the expected limit properties and
   `NeedDaemonReload=no`.
3. Root owns each limit file of the slice cgroup, with no group or other write.
   Each file holds the expected value.
4. When a unit uses an I/O control, the `cgroup.controllers` file in the root
   cgroup of the user manager lists `io`.
5. The `cron` and `at` checks of the trust model pass.

A missing item or a different value fails closed. The error names the drop-in
path and the property or file that differs. Profiles without limits do not need
the host cap.

### Reserve and pressure

The host cap limits the supervisor, the proxy and every workload of the UID
together. Setup rejects unit limits that do not leave a reserve under the host
cap. Unit `MemoryHigh` and unit `MemoryMax` must each be at most the slice
`MemoryHigh` minus the memory reserve. Each other unit control must be at most
the value of the same control in the host cap, minus its reserve.

When a unit with limits does not set a control that the host cap sets, setup
fills in a default and shows it. For every control other than `MemoryHigh` and
`MemoryMax`, the default is the host cap value minus the reserve.

The memory defaults do not use the slice `MemoryMax`. Both stay at most the
slice `MemoryHigh` minus the memory reserve. That bound is the memory ceiling.
Setup rounds the slice `memory.current` up to the next MiB. The default
`MemoryMax` is the ceiling minus that usage. The default `MemoryHigh` is the
default `MemoryMax` minus the memory reserve when the result is at least
256 MiB.

Otherwise setup leaves `MemoryHigh` unset. If the default `MemoryMax`
is below 256 MiB, setup fills in no memory value and shows the memory-pressure
error.

With the example host cap, the ceiling is 55296 MiB. The slice `MemoryMax`
minus the reserve is 59392 MiB, which is above the ceiling, so it is not a
default. A `MemoryMax` of 55296 MiB also fails admission while the slice uses
any memory.

Subtracting `memory.current` leaves room for that usage. At
1024 MiB of slice usage the defaults are `MemoryMax` 54272 MiB and
`MemoryHigh` 52224 MiB. Then 1024 plus 54272 plus 2048 equals the slice
`MemoryHigh` of 57344, so admission can accept the unit.

A later rise in `memory.current` still fails the admission check above. The
defaults are a starting point.
[Memory sizing for mapped models](#memory-sizing-for-mapped-models)
still requires `MemoryHigh` above the model file size plus the measured
working set.

| Control | Proposed reserve |
| --- | --- |
| `MemoryHigh`, `MemoryMax` | 2048 MiB |
| `MemorySwapMax` | 0 |
| `TasksMax` | 512 |
| `CPUQuota` | 100% |
| I/O bandwidth and IOPS caps | 10% of the host cap |

Memory can stay charged to the slice after a unit stops. Examples are files in
`/dev/shm` and other `tmpfs` mounts. The kernel guide states that the kernel
charges a memory area to the cgroup that instantiated it, until a process
releases the area. Before admission of a unit with limits, the supervisor reads
`memory.current` of the slice. The value plus the `MemoryMax` of the unit plus
the reserve must be at most the slice `MemoryHigh`. Otherwise admission fails
closed, and the error names `/dev/shm` and `tmpfs` as common causes.

A unit with limits renders `OOMScoreAdjust=500`. An OOM kill at the slice level
then selects a workload before the supervisor and the proxy. From systemd sources, systemd sets
`memory.oom.group` to 1 only for `OOMPolicy=kill`
([v252](https://github.com/systemd/systemd/blob/v252/src/core/service.c#L778),
[v255](https://github.com/systemd/systemd/blob/v255/src/core/service.c#L846),
[v259](https://github.com/systemd/systemd/blob/v259/src/core/service.c#L909)).
The required `OOMPolicy=stop` keeps it at 0. Instead, systemd stops the
remaining processes of the unit after the first kill.

The design does not render `RemoveIPC=` or `PrivateIPC=`. The systemd manual
states that `RemoveIPC=` has an effect only with `User=`, `Group=` or
`DynamicUser=`, and that `PrivateIPC=` has no effect on POSIX shared memory. In
a user manager, every unit runs as the same UID.

## Controller checks

The user manager receives only the delegated controllers. The upstream unit sets
`Delegate=pids memory cpu` at all three versions
([v252](https://github.com/systemd/systemd/blob/v252/units/user%40.service.in#L24),
[v255](https://github.com/systemd/systemd/blob/v255/units/user%40.service.in#L24),
[v259](https://github.com/systemd/systemd/blob/v259/units/user%40.service.in#L28)).
The `io` controller needs the second drop-in of the host cap.

From systemd sources, the user manager reads its supported controllers from
`cgroup.controllers` in its root cgroup
([v252](https://github.com/systemd/systemd/blob/v252/src/basic/cgroup-util.c#L1967),
[v255](https://github.com/systemd/systemd/blob/v255/src/basic/cgroup-util.c#L2052),
[v259](https://github.com/systemd/systemd/blob/v259/src/basic/cgroup-util.c#L1659)).
It removes unsupported controllers from each unit's mask
([v252](https://github.com/systemd/systemd/blob/v252/src/core/cgroup.c#L1919),
[v255](https://github.com/systemd/systemd/blob/v255/src/core/cgroup.c#L2213),
[v259](https://github.com/systemd/systemd/blob/v259/src/core/cgroup.c#L1852)).
The unit still starts, and systemd does not apply the limit.

systemd only logs a failed attribute write
([v252](https://github.com/systemd/systemd/blob/v252/src/core/cgroup.c#L109),
[v255](https://github.com/systemd/systemd/blob/v255/src/core/cgroup.c#L115),
[v259](https://github.com/systemd/systemd/blob/v259/src/core/cgroup.c#L126)).
`systemctl show` reports the configured value in both cases. Thus a loaded
property is not enforcement evidence.

| Controller | Controls |
| --- | --- |
| `memory` | `MemoryHigh`, `MemoryMax`, `MemorySwapMax` |
| `pids` | `TasksMax` |
| `cpu` | `CPUWeight`, `CPUQuota` |
| `io` | `IOReadBandwidthMax`, `IOWriteBandwidthMax`, `IOReadIOPSMax`, `IOWriteIOPSMax`, `IOWeight` |

The checks separate two different failures:

- **Controller not enabled.** The unit's own `cgroup.controllers` does not list
  the controller, so the kernel shows none of its files. This is a delegation
  failure. The setup error names the controller and the host cap step.
- **Interface file missing.** The unit's `cgroup.controllers` lists the
  controller, but one file is absent. An example is `memory.swap.max` when
  kernel swap accounting is off. From systemd sources, systemd treats a missing
  `memory.swap.max` as an expected case
  ([v252](https://github.com/systemd/systemd/blob/v252/src/core/cgroup.c#L397),
  [v255](https://github.com/systemd/systemd/blob/v255/src/core/cgroup.c#L438),
  [v259](https://github.com/systemd/systemd/blob/v259/src/core/cgroup.c#L403)).
  The setup error names the file and the kernel feature.

Both failures fail closed.

Swap accounting needs a check before setup accepts the unit, because setup adds
`MemorySwapMax=0` when the unit has `MemoryMax`. Setup reads `memory.swap.max`
in the root cgroup of the user manager. If the file is absent, setup rejects the
unit. The error names the automatic `MemorySwapMax=0` and tells the operator to
enable swap accounting or to remove `MemoryMax`.

## Unit limits

Limits belong to the owned unit, not to the profile. Adopted units get no
limits. To limit one, an operator converts it to an owned unit through setup
preview and apply. Trainers come first, then inference units.

Profiles that share one owned Ollama unit each carry a copy of the owned launch
specification (`nativeModel.owned`). The catalog validator requires equal
specifications for a shared unit. The `limits` type must be a comparable value
struct with no pointer, slice or map field. Then the existing equality check and
catalog clone stay correct. Tests must cover two shared profiles with equal
limits and with different limits.

| Control | Catalog value | Readback file and value |
| --- | --- | --- |
| `MemoryHigh` | MiB | `memory.high`, bytes |
| `MemoryMax` | MiB | `memory.max`, bytes |
| `MemorySwapMax` | MiB. 0 when the unit has `MemoryMax` and no swap value | `memory.swap.max`, bytes |
| `TasksMax` | Count | `pids.max` |
| `CPUWeight` | 1 to 10000 | `cpu.weight` |
| `CPUQuota` | Percent | `cpu.max`, `P*1000 100000` |
| `IOReadBandwidthMax`, `IOWriteBandwidthMax` | Bytes per second, one device | `io.max` line of the device |
| `IOReadIOPSMax`, `IOWriteIOPSMax` | Operations per second, one device | `io.max` line of the device |
| `IOWeight` with iocost | 1 to 10000 | `io.weight`, `default N` |
| `IOWeight` with BFQ | 1 to 10000 | `io.weight`, `default N`, and `io.bfq.weight`, `default B` |

MiB values are multiples of the page size, and systemd uses base 1024 for the
`M` suffix. For `CPUQuota`, the readback uses the default period
([v252](https://github.com/systemd/systemd/blob/v252/src/core/cgroup.c#L968),
[v255](https://github.com/systemd/systemd/blob/v255/src/core/cgroup.c#L1216),
[v259](https://github.com/systemd/systemd/blob/v259/src/core/cgroup.c#L1146)).
The pids controller counts tasks, including threads. CUDA runtimes create many
threads, so a low `TasksMax` stops normal startup.

`MemorySwapMax` needs `MemoryMax` in this design. This is a deliberate
narrowing of the issue. The value struct cannot show the difference between an
absent value and 0. With this rule, both give the same rendering.

### I/O devices

From systemd sources, systemd resolves a device path to the originating device
of a device-mapper stack, then to the whole disk
([v252](https://github.com/systemd/systemd/blob/v252/src/core/cgroup.c#L855),
[v255](https://github.com/systemd/systemd/blob/v255/src/core/cgroup.c#L1101),
[v259](https://github.com/systemd/systemd/blob/v259/src/core/cgroup.c#L1034)).
If the lookup fails, systemd skips the limit with a warning
([v252](https://github.com/systemd/systemd/blob/v252/src/core/cgroup.c#L1169),
[v255](https://github.com/systemd/systemd/blob/v255/src/core/cgroup.c#L1417),
[v259](https://github.com/systemd/systemd/blob/v259/src/core/cgroup.c#L1265)).

Thus setup accepts only a whole disk that is not a device-mapper device. The
operator selects the disk. Setup resolves it to its major and minor numbers. It
requires `S_ISBLK`, no `partition` file and no `dm` directory under
`/sys/dev/block/MAJ:MIN`. Setup reads the kernel name from `DEVNAME` in the
`uevent` file of that directory.

The catalog stores the canonical path `/dev/<kernel name>` and the numbers. The
renderer writes that canonical path, so systemd and the checks resolve the same
node. Each launch checks that the path is a root-owned block device node with
the recorded numbers. The launch parser compares the device token byte for byte
with the catalog value.

systemd writes all four keys on the device line, with `max` for a key that the
unit does not set. The readback requires the exact line. An example for device
259:0 with only a read bandwidth cap:

```text
259:0 rbps=524288000 wbps=max riops=max wiops=max
```

`IOWeight` has an effect only when the device uses the BFQ scheduler or the
iocost controller. The kernel guide names these two as the providers of weight
distribution. Setup reads `/sys/dev/block/MAJ:MIN/queue/scheduler` for
`[bfq]` and the root `io.cost.qos` line for `enable=1`. Setup reports which one
applies, or rejects `IOWeight` when neither applies.

With BFQ, systemd also writes `io.bfq.weight` with a value in the range 1 to
1000
([v252](https://github.com/systemd/systemd/blob/v252/src/core/cgroup.c#L1073),
[v255](https://github.com/systemd/systemd/blob/v255/src/core/cgroup.c#L1321),
[v259](https://github.com/systemd/systemd/blob/v259/src/core/cgroup.c#L1190)).
From systemd sources, the value B is N for N up to 100, and
`100 + (N - 100) * 900 / 9900` in integer arithmetic above 100
([v252](https://github.com/systemd/systemd/blob/v252/src/basic/cgroup-util.h#L138),
[v255](https://github.com/systemd/systemd/blob/v255/src/basic/cgroup-util.h#L139),
[v259](https://github.com/systemd/systemd/blob/v259/src/basic/cgroup-util.h#L107)).
With BFQ, the readback requires both files. With iocost, it requires
`io.weight` only.

## Rendering and grammar

A unit with limits always renders `OOMPolicy=stop`. The default comes from
`DefaultOOMPolicy=`, which is `stop`, but the user manager reads it from its own
configuration
([v252](https://github.com/systemd/systemd/blob/v252/src/core/main.c#L662),
[v255](https://github.com/systemd/systemd/blob/v255/src/core/main.c#L686),
[v259](https://github.com/systemd/systemd/blob/v259/src/core/main.c#L811)).
An explicit value removes the dependency on host configuration.

The renderer adds these lines directly after `Restart=no`. It writes only the
configured lines, in this order:

```ini
OOMPolicy=stop
OOMScoreAdjust=500
MemoryHigh=<MiB>M
MemoryMax=<MiB>M
MemorySwapMax=<MiB>M
TasksMax=<count>
CPUWeight=<weight>
CPUQuota=<percent>%
IOWeight=<weight>
IOReadBandwidthMax=/dev/<kernel name> <bytes>
IOWriteBandwidthMax=/dev/<kernel name> <bytes>
IOReadIOPSMax=/dev/<kernel name> <count>
IOWriteIOPSMax=/dev/<kernel name> <count>
```

The strict launch parser accepts these lines only through the owned-unit path:
`parseOwnedLaunchUnit` for owned inference units, and the exact trainer form for
trainers. The parser removes the exact rendered block before the shared grammar
reads the rest, as it does for owned dependency lines. The adopted and imported
launch grammar does not accept any limit directive. The feature uses no
drop-ins on owned units.

## Drift checks

All checks fail closed.

1. **Before setup accepts a unit with limits and before each launch.** The
   `cgroup.controllers` file in the root cgroup of the user manager lists each
   required controller. The checks of the host cap pass.
2. **Before admission.** The memory pressure check of the slice passes.
3. **At launch.** The loaded unit properties match the catalog, `OOMPolicy`
   and `OOMScoreAdjust` included. `DropInPaths` is empty, and
   `NeedDaemonReload=no`.
4. **After start and at each reconcile.** The unit's cgroup files hold the
   exact values in the table above.

The readback in item 4 runs while the recursive cgroup of the unit contains
processes. This includes a trainer in the `exited` state with remaining
processes. If the cgroup is empty or absent, the supervisor does not judge
drift. The existing terminal evidence and release rules then decide the
outcome.

The readback has two failure types. A value mismatch is drift. A read error or
a missing file is a readback failure. After a readback failure, the supervisor
reads `ActiveState`, `SubState`, `InvocationID` and `MainPID` again. The
failure counts only when the same invocation is still active or exited with a
populated cgroup. Otherwise the terminal evidence rules decide the outcome.

A unit test covers a cgroup that goes away between the state check and the
read.

Drift and a counted readback failure stop the unit, and the error names the
type. The results are:

- **Trainer.** The command that finds the failure stops the unit and does not
  retry the launch. That command is `training-start` after start, or
  `training-reconcile` later. Reconcile still records no outcome, so the job
  stays pending. This is a documented exception to the rule that reconcile only
  reads evidence. Explicit `training-recover` then verifies release and records
  `interrupted` when no verified outcome exists. Drift never gives `completed`.
  The implementation updates `docs/TRAINING.md` to match.
- **Inference.** The supervisor stops the unit. The activation or the stable
  state fails under the existing health, transition and recovery rules.

## Outcome classes

A resource threshold is not always a kill.

| Signal | Class | Evidence | Effect on the unit |
| --- | --- | --- | --- |
| Usage above `MemoryHigh` | Throttle | `memory.events` `high` | Reclaim and delay. The OOM killer does not run |
| Usage at `MemoryMax`, reclaim succeeds | Throttle | `memory.events` `max` | Delay. File-backed model pages can refault |
| Usage at `MemoryMax`, reclaim fails | Kill | `memory.events` `oom_kill`, `Result=oom-kill` | Stop with `OOMPolicy=stop` |
| Some kernel allocations at `MemoryMax` | Allocation failure | `ENOMEM` in the process | The process decides, often an exit |
| Fork or thread creation at `TasksMax` | Task-creation failure | `pids.events` `max`, `EAGAIN` | The process decides. No kill |
| CPU use at `CPUQuota` | Throttle | `cpu.stat` `nr_throttled` | Delay only |
| I/O at a bandwidth or IOPS cap | Throttle | `io.stat` | Delay only |
| `CPUWeight`, `IOWeight` | Share | None | Delay under contention only |

Sources: the kernel guide sections for `memory.high`, `memory.max`,
`memory.events`, `pids.max`, `cpu.max` and `io.max`.

From systemd sources, an OOM kill with `OOMPolicy=stop` stops a running or
exited service with the `oom-kill` result
([v252](https://github.com/systemd/systemd/blob/v252/src/core/service.c#L3415),
[v255](https://github.com/systemd/systemd/blob/v255/src/core/service.c#L3648),
[v259](https://github.com/systemd/systemd/blob/v259/src/core/service.c#L4049)).

The supervisor makes decisions from systemd unit state and the existing
evidence contracts only. The event counters are diagnostics. They are never
completion or release evidence. A counter also goes away when systemd removes
the empty cgroup.

## Trainers

- A throttled trainer stays active. Reconcile reports that the trainer did not
  exit. Throttling never gives completion.
- A fenced `oom-kill` result is terminal `failed` evidence. The same applies to
  `exit-code`, `signal` and `core-dump`. A task-creation or allocation failure
  normally becomes one of these results.
- `OOMPolicy=stop` also covers the `RemainAfterExit=yes` exited state. An OOM
  kill of a remaining process after the adapter exits still gives
  `Result=oom-kill`. If reconcile did not bind an outcome before, the systemd
  failure wins over a `completed` terminal file.
- A limit kill is a job outcome. It is not an error latch. After release
  verification, admission returns to idle under the existing training rules.

## Inference services

Inference keeps its existing health, transition and recovery rules. A limit
kill stops the owned unit, and the health check then fails. A throttle can delay
readiness or a response. The existing health deadlines and proxy terminal
evidence apply without change. Inference adds no new state, phase or result.

## Memory sizing for mapped models

llama.cpp maps the GGUF file of a model into memory.

The kernel guide states that the kernel charges a memory area to the cgroup
that instantiated it. Thus the kernel charges the page cache of a model to the
unit that reads the model first. A `MemoryHigh` below the model size plus the
working set causes reclaim and refaults during the load. The load can then
exceed the readiness deadline without a kill.

Set `MemoryHigh` above the model file size plus the measured working set, and
set `MemoryMax` above `MemoryHigh`. Measure both on the host during
qualification.

## Checks before admission release

A limit kill is not release evidence. After any termination, the existing
release checks stay independent and each is necessary:

1. The unit is inactive, and the recursive cgroup is empty or absent.
2. No GPU compute process remains on the bound GPU.
3. Capacity is available.

A process in an uninterruptible driver wait can keep the cgroup populated after
the kill. Admission then stays closed. Failed cleanup stays fail-closed under
the existing recovery rules.

## Lifecycle

`Restart=no` stays. The feature adds no `StartLimit*` policy, no automatic
restart and no lifecycle phase. Apply in setup already requires verified idle
release, so a limit change never reaches a running unit. This matters because
`daemon-reload` applies new cgroup values to a running unit.

## Desktop

The setup application shows limits on the existing **Resource checks** page of
**Advanced settings**. Limits are off by default for each unit. When the user
enables limits, setup fills in the defaults from
[Reserve and pressure](#reserve-and-pressure). The default `MemoryMax` is the
slice `MemoryHigh` minus the reserve minus the slice usage. It is not the
slice `MemoryMax` minus the reserve.

The page shows memory in GiB,
CPU in cores and tasks as a count. I/O controls and `IOWeight` are in a
separate, closed section.

The page shows one host cap status:

| Status | Text | Action |
| --- | --- | --- |
| Present | Host protection is active for this account | None |
| Absent | This account has no host protection. An administrator must run one step as root | Show the exact drop-ins and commands, with a copy button |
| Different | Host protection settings do not match. An administrator must update them | Show each different value and the exact step |

Each fail-closed error class has plain-language text:

| Error class | Text |
| --- | --- |
| Controller not enabled | This computer does not let the account limit `<controller>`. An administrator must change the account settings |
| Interface file missing | The system cannot apply `<control>`. `<kernel feature>` is off on this computer |
| Host cap absent or different | The host protection of this account is missing or different. See the step on this page |
| Reserve violation | This limit leaves too little room for the supervisor. Use `<value>` or less |
| Escape route | Programs from this account can run outside host protection through `<route>` |
| Memory pressure | Other memory of this account is still in use. Close the programs that use `/dev/shm`, then try again |
| Drift or readback failure | The limits changed after start. The supervisor stopped the workload |
| `IOWeight` not applicable | The selected disk does not support I/O weight |

## Schema

- Catalog version 5 accepts all content of version 4, which includes the
  version 2 owned launches. It adds `limits` on the owned launch specification
  (`nativeModel.owned`) and on the trainer binding.
- In version 5, a trainer binding with limits uses binding version 2. A trainer
  binding without limits keeps binding version 1 and its exact bytes. Version 4
  accepts only binding version 1. The job file and terminal file versions stay 1.
- `limits` is strict JSON with bounded values. It has a `version` field and
  needs at least one control. The decoder rejects unknown fields and an empty or
  all-zero `limits` object, so an absent object is the only legacy form.
- Setup writes version 5 only when at least one unit has limits. Otherwise
  setup keeps the exact legacy bytes and fingerprints.
- Limits pass through render, spec, fingerprint and rollback verification.
- The catalog version table in `docs/WORKLOADS.md` gets a version 5 row.

Every catalog reader must accept version 5:

- `Validate` of `control.Catalog` accepts version 5. Its gate for trainer
  bindings accepts version 4 and version 5.
- The proxy reads the catalog without `Validate`. Its `supportedCatalogVersion`
  guard must accept version 5. A test must send a version 5 catalog through the
  proxy.
- The store guard for version changes already permits a change to version 5.
  It gets one new rule: it rejects a change from version 5 to a lower version
  when the new catalog still carries `limits`.

Every catalog writer uses one version rule:

- One Go function computes the lowest version that a catalog needs. The result
  is 5 with any limits, 4 with a trainer binding, 2 with an owned launch, and 1
  otherwise. Setup preview and apply both use it.
- The function replaces the client rule in `clients/setup/setup-window.mjs`,
  which only raises the version. It also replaces the literal version 2 in
  `internal/setup/owned.go`.
- Drafts get a strict `limits` field. The draft file version goes up, so older
  binaries reject a draft with limits and do not drop the field.
- The test of version 2 to version 5 and back to the same version 2 bytes uses
  this function.

Older binaries reject version 5. To roll back, an operator first removes the
limits through setup and confirms that the catalog version went down. Only then
does the operator install the older binary. A backup restore is the other path.
The implementation adds these steps to `docs/RESTORING.md`.

When the catalog before the limits also used its lowest version, the result has
the exact earlier bytes. A catalog with a higher version than it needs gets the
lower version at the next setup write. Its unit bytes and fingerprints stay the
same.

Proposed bounds:

| Field | Range | Rule |
| --- | --- | --- |
| `memoryHighMiB` | 256 to 16777216 | Less than `memoryMaxMiB` when the unit has both |
| `memoryMaxMiB` | 256 to 16777216 | None |
| `memorySwapMaxMiB` | 0 to 16777216 | Needs `memoryMaxMiB`. 0 when absent |
| `tasksMax` | 64 to 4194304 | None |
| `cpuWeight` | 1 to 10000 | None |
| `cpuQuotaPercent` | 1 to 102400 | None |
| `ioDevice` | `^/dev/[a-z][a-z0-9]{0,31}$` | Required with any I/O field. Setup writes the canonical kernel name |
| `ioDeviceNumber` | `MAJ:MIN` | Required with `ioDevice`. Setup writes it |
| `ioReadBandwidthMax`, `ioWriteBandwidthMax` | 1048576 to 1099511627776 | Bytes per second |
| `ioReadIOPSMax`, `ioWriteIOPSMax` | 100 to 10000000 | Operations per second |
| `ioWeight` | 1 to 10000 | Needs BFQ or iocost on `ioDevice` |

## Qualification procedure

Run each probe as the service UID on hosts with systemd 252, 255 and 259. Install
`stress-ng` from the distribution package first. Use the cgroup of each probe
unit, which `systemctl --user show -P ControlGroup` gives. Record the systemd,
kernel and `stress-ng` versions and the distribution in the private host
evidence, not in this record. Read each cgroup file while the probe unit is
active. Stop and reset each probe unit after the check.

| Claim | Probe | Expected result |
| --- | --- | --- |
| Default delegation | `cat` the `cgroup.controllers` file of the user manager root. Run `systemctl show -p DropInPaths user-UID.slice` | `cpu memory pids`, and `io` after the host cap. `DropInPaths` lists the vendor `10-defaults.conf` |
| A missing controller is silent | Before the `io` drop-in: `systemd-run --user --unit=gws-q-io -p IOReadBandwidthMax="/dev/DISK 1M" sleep 600` | The unit is active. `io.max` is absent. `systemctl --user show -p IOReadBandwidthMax gws-q-io` gives `IOReadBandwidthMax=/dev/DISK 1048576` |
| Values apply | `systemd-run --user --unit=gws-q-mem -p MemoryHigh=256M -p MemoryMax=512M -p MemorySwapMax=0 -p TasksMax=64 -p CPUQuota=50% -p OOMPolicy=stop -p OOMScoreAdjust=500 sleep 600` | `memory.high` 268435456, `memory.max` 536870912, `memory.swap.max` 0, `pids.max` 64, `cpu.max` `50000 100000` |
| Loaded properties | `systemctl --user show -p MemoryMax -p TasksMax -p CPUQuotaPerSecUSec -p OOMPolicy -p OOMScoreAdjust gws-q-mem` | `MemoryMax=536870912`, `TasksMax=64`, `CPUQuotaPerSecUSec=500ms`, `OOMPolicy=stop`, `OOMScoreAdjust=500` |
| A bus client rejects an invalid value | `systemd-run --user -p MemoryMax=bogus true` | A nonzero exit status and no unit |
| Memory throttle | `systemd-run --user --unit=gws-q-high -p MemoryHigh=64M -p MemorySwapMax=0 stress-ng --vm 1 --vm-bytes 128M --timeout 30s` | During the run: `ActiveState=active`, `memory.events` `high` above 0 and `oom_kill` 0 |
| CPU throttle | `systemd-run --user --unit=gws-q-cpu -p CPUQuota=10% stress-ng --cpu 1 --timeout 30s` | During the run: `cpu.stat` `nr_throttled` above 0 |
| OOM kill | `systemd-run --user --unit=gws-q-oom -p MemoryMax=64M -p MemorySwapMax=0 -p OOMPolicy=stop stress-ng --vm 1 --vm-bytes 256M --timeout 60s` | `Result=oom-kill` and `ActiveState=failed` |
| OOM kill in the exited state | `systemd-run --user --unit=gws-q-exited -p Type=exec -p RemainAfterExit=yes -p MemoryMax=64M -p MemorySwapMax=0 -p OOMPolicy=stop sh -c 'stress-ng --vm 1 --vm-bytes 256M --timeout 60s & exit 0'` | After the main process exits with status 0: `Result=oom-kill` and `ActiveState=failed` |
| Task-creation failure | `systemd-run --user --unit=gws-q-pids -p TasksMax=16 sh -c 'for i in $(seq 64); do (sleep 600 &) 2>/dev/null \|\| true; done; exec sleep 600'` | During the run: `ActiveState=active` and `pids.events` `max` above 0 |
| Host cap holds | As the service UID, write `max` to each capped file of the slice and of `user@UID.service`: `memory.high`, `memory.max`, `memory.swap.max`, `pids.max`, `cpu.max` and `io.max` | Each write fails with `EACCES` |
| BFQ weight | On a BFQ device: `systemd-run --user --unit=gws-q-bfq -p IOWeight=1000 sleep 600` | `io.weight` `default 1000` and `io.bfq.weight` `default 181` |

Each test category of #584 maps to one level:

| Test category | Level |
| --- | --- |
| Rendered values, fingerprint stability, legacy bytes, rollback bytes | Unit tests with golden files |
| Loaded values, drift, missing controllers, missing host cap, known drop-ins | Unit tests with a fake command runner and a fixture cgroup tree, then host qualification |
| Throttling without false completion, terminal failure | Unit tests with recorded unit states, then the throttle and OOM probes above |
| Cleanup and recovery failure | Unit tests with a fixture cgroup tree that stays populated |
| Applied cgroup values, OOM kill, host cap ownership | Host qualification only. These need a real user manager |
