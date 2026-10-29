# Host-protection resource limits (#584)

[Documentation](README.md) | [Repository](../README.md)

This record gives the design for
[#584](https://github.com/mickey-kras/gpu-workload-supervisor/issues/584).
The current release has only the schema part of this feature:

- Catalog version 5 accepts `limits` on an owned inference launch
  specification. The renderer and the strict parser carry them.
- The supervisor refuses a switch to a unit with limits before it stops the
  current workload. The runtime also does not start the unit or report it as
  healthy. This gate does not change other workloads, preflight or release
  checks.
- Setup cannot write limits. It rejects a request with limits before it writes
  a unit, and it accepts a request that removes them.

Later changes add the other parts of this design. The current release does not
have trainer binding limits or draft and setup support for limits. It also does
not have the **Resource checks** page, the host cap, or the controller and
cgroup readback checks. `requiredMiB` stays a VRAM demand estimate before launch, not
an enforced ceiling. GPU VRAM enforcement is out of scope.

## Method

Statements marked "from systemd sources" come from the source files at tags
`v252`, `v255` and `v259`. The links give the exact lines. Kernel statements
come from the [cgroup v2 guide](https://docs.kernel.org/admin-guide/cgroup-v2.html).
The kernel guide has no version pin, so qualification on each host kernel must
confirm those statements. No running user manager was available, so the spike
started no probe unit. The [qualification procedure](#qualification-procedure)
gives the host checks that must confirm each claim before an operator enables
limits.

This record keeps one offline parse check. It passes valid and invalid values
the same way. With systemd 255, `systemd-analyze verify` printed no warning and
exited with status 0 for the unit below. The same command with
`MemoryMax=bogus` additionally exited 0 with only a warning. Thus the renderer
must emit only validated values, and the loaded-property check must confirm
each value.

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
It can stop the supervisor, edit the unit files and reload the user manager.
The [security policy](../SECURITY.md) already treats the service UID as
trusted, so unit limits are protection against accidents, not a security
boundary. Isolation between workloads with separate service accounts is out of
scope.

The slice contains `user@UID.service` and the session scopes of the UID, for
example from `sshd` or a display manager
([v252](https://github.com/systemd/systemd/blob/v252/man/user%40.service.xml#L59),
[v255](https://github.com/systemd/systemd/blob/v255/man/user%40.service.xml#L59),
[v259](https://github.com/systemd/systemd/blob/v259/man/user%40.service.xml#L60)).
From systemd sources, delegation makes only `cgroup.procs`,
`cgroup.subtree_control` and `cgroup.threads` writable by the user. At `v259`,
`memory.oom.group` and `memory.reclaim` are additionally writable
([v252](https://github.com/systemd/systemd/blob/v252/src/shared/cgroup-setup.c#L399),
[v255](https://github.com/systemd/systemd/blob/v255/src/shared/cgroup-setup.c#L400),
[v259](https://github.com/systemd/systemd/blob/v259/src/shared/cgroup-setup.c#L194)).
systemd does not delegate the slice, so root owns all of its limit files. The
kernel guide states that a delegatee cannot escape the limits of its parent.

Setup does not try to prove that no escape exists. It checks the common routes
it can see:

| Route | Check | Result |
| --- | --- | --- |
| `cron` | Spools, system crontabs, and cronie allow or deny rules, as below | Fail closed, and the error names the file or spool |
| `at` | The same allow or deny rule, plus queued jobs, as below | Fail closed, and the error names the file or job |
| Groups and sockets | Membership in `docker`, `podman`, `libvirt`, `lxd`, `incus`, `sudo`, `wheel` or `disk`, or a daemon socket this UID can open | Fail closed, and the error names each group or socket |
| `sudo` | Any rule from `sudo -l -U`, or a policy setup cannot read | Fail closed, and the error names each rule or that setup cannot read the policy |
| `polkit` | A rule that lets this UID start a system unit, or a policy setup cannot read | Fail closed, and the error names the rule or that setup cannot read the policy |
| System units | A loaded system unit with `User=` set to the service UID | Fail closed, and the error names each unit |

The cron check does not stop at the daemon. Setup fails closed when a user
crontab exists under `/var/spool/cron` or `/var/spool/cron/crontabs`. It also
fails when `/etc/crontab` or a file in `/etc/cron.d` runs as this UID by name
or numeric id. An absent cron binary does not skip that scan. The error names
the spool or the file.

Allow and deny files, and every parent directory, must be root-owned regular
files. They must not have group or other write, or an ACL that lets this UID
write. Setup walks them with `O_NOFOLLOW`.

An unreadable `cron.allow` counts as absent. A
line counts only when it is the precise user name and ends in a newline, with no
trailing space or CR. The check passes only when those spools and system
entries are empty. It also needs either a readable `cron.allow` that omits the
user, or only `cron.deny` listing the user.

The `at` check uses the same allow and deny rule, with `at.allow` and
`at.deny`. Setup additionally fails closed when a queued job for this UID exists. An
absent `at` binary does not skip that scan. The error names the allow file or
the queued job.

A socket check reads the mode and ACL of the docker, podman, and libvirt
sockets. Group membership alone is not the whole check.

## Host cap

The host cap is one manual root step with two system drop-ins. The UID 1000
sample below shows them: the first drop-in caps the slice, the second
delegates `io` to the user manager.

```ini
# /etc/systemd/system/user-1000.slice.d/50-gws-host-cap.conf
[Slice]
MemoryHigh=57344M
MemoryMax=61440M
MemorySwapMax=0
TasksMax=16384
CPUQuota=1400%
IOReadBandwidthMax=/dev/nvme0n1 1000000000
IOWriteBandwidthMax=/dev/nvme0n1 500000000
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

To install or change the drop-ins, the operator first stops all work: the
supervisor must report verified idle release, and external service management
stays paused. Then the administrator runs `systemctl daemon-reload`.

The slice limits apply at the reload. Only a new `Delegate=` value needs a
restart of `user@UID.service`, which stops the supervisor and every owned unit.
If a restart occurs without idle release, the existing [boot
reconciliation](OPERATIONS.md#boot-and-explicit-recovery) rules apply.

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
after `10-defaults.conf`. systemd reads drop-ins in name order (per the
`systemd.unit` manual), so the host-cap `TasksMax` replaces the vendor
`TasksMax=33%`. The checks of the effective properties and cgroup files below
decide the result, whatever drop-ins exist. Unit tests use a fixture with the
vendor drop-ins and a fake package manager.

### Host cap checks

Every unit with limits needs the host cap. Setup shows the precise drop-ins with
the selected values. Setup and runtime preflight check these items before setup
accepts a unit with limits and before each launch of one:

1. Each drop-in of the host cap is a regular file, not a symlink. The walk
   uses `O_NOFOLLOW`. Root owns the file and every parent directory through
   `/etc`. Every parent is a real directory. None has group or other write, or
   an ACL or capability that lets the service UID write.
2. `systemctl show` for the slice and for `user@UID.service` lists only known
   drop-ins in `DropInPaths` and reports the expected limit properties and
   `NeedDaemonReload=no`.
3. The slice path comes only from the system manager `ControlGroup`. Root owns
   each limit file there, with no group or other write and no ACL for the
   service UID. The user-manager cgroup and the unit cgroup are descendants of
   that slice. The slice shows a finite `memory.max`, a finite `pids.max`, a
   finite `cpu.max`, and `memory.swap.max` of 0. When swap is off and swap
   accounting is impossible, setup accepts a missing file.
4. When a unit uses an I/O control, the `cgroup.controllers` file in the root
   cgroup of the user manager lists `io`.
5. Every escape check of the trust model passes, including `cron` and `at`.

A missing item or a different value fails closed. The error names the drop-in
path and the property or file that differs. Profiles without limits do not need
the host cap.

### Reserve and pressure

The host cap limits the supervisor, the proxy and every workload of the UID
together. Setup rejects unit limits that do not leave a reserve under the host
cap. Unit `MemoryHigh` and unit `MemoryMax` must each be at most the slice
`MemoryHigh` minus the memory reserve. Each other unit control must be at most
the same control's host-cap value minus its reserve.

When a unit with limits does not set a control that the host cap sets, setup
fills in a default and shows it. Setup does not fill `MemoryHigh` or
`MemoryMax`: the operator sets both from the measured model size. `MemoryMax`
must stay above `MemoryHigh`, and both must stay at most the memory ceiling.

For every other control, the default is the host cap value minus the reserve.
Setup rounds a percent reserve down to a whole byte or a whole operation before
that subtraction. If the default is outside the schema range, setup rejects the
unit and writes no `limits` for it. Setup does not store 0: the value struct
cannot tell 0 from an absent field. The same rounding is the maximum setup
accepts.

The memory ceiling is the slice `MemoryHigh` minus the memory reserve. It does
not use the slice `MemoryMax`. With the example host cap, the ceiling is
55296 MiB. The slice `MemoryMax` minus the reserve is 59392 MiB, which is above
the ceiling and therefore not a legal unit value. A unit `MemoryMax` of
55296 MiB additionally fails admission while the slice uses any memory.

Setup reads `memory.current` before it accepts memory values. A failed read
fails closed. Setup does not treat it as zero. The page shows the measured
usage and the ceiling next to the fields. The operator's `MemoryMax` must leave
room for that usage under the admission check below. [Memory sizing for mapped
models](#memory-sizing-for-mapped-models) still requires `MemoryHigh` above the
model file size plus the measured working set.

| Control | Proposed reserve |
| --- | --- |
| `MemoryHigh`, `MemoryMax` | 2048 MiB |
| `MemorySwapMax` | 0 |
| `TasksMax` | 512 |
| `CPUQuota` | 100% |
| I/O bandwidth and IOPS caps | 10% of the host cap, rounded down |

Memory can stay charged to the slice after a unit stops, for example files in
`/dev/shm` and other `tmpfs` mounts. The kernel guide states that the kernel
charges a memory area to the cgroup that instantiated it, until a process
releases the area.

Before admission of a unit with limits, the supervisor reads `memory.current`
of the slice. A failed read fails closed. Setup does not use zero for that
read. The value plus the unit's `MemoryMax` plus the reserve must be at most
the slice `MemoryHigh`. Otherwise admission fails closed. The error names the
measured usage, the ceiling, and `/dev/shm` and `tmpfs` as common causes.

Admission does not divide the ceiling across units: a second unit launches only
when its own `MemoryMax` still fits beside the memory already charged.

A unit with limits renders `OOMScoreAdjust=500`, so an OOM kill at the slice
level selects a workload before the supervisor and the proxy. From systemd
sources, systemd sets `memory.oom.group` to 1 only for `OOMPolicy=kill`
([v252](https://github.com/systemd/systemd/blob/v252/src/core/service.c#L778),
[v255](https://github.com/systemd/systemd/blob/v255/src/core/service.c#L846),
[v259](https://github.com/systemd/systemd/blob/v259/src/core/service.c#L909)).
The required `OOMPolicy=stop` keeps it at 0. Instead, systemd stops the
remaining processes of the unit after the first kill.

The design does not render `RemoveIPC=` or `PrivateIPC=`. The systemd manual
states that `RemoveIPC=` has an effect only with `User=`, `Group=` or
`DynamicUser=`, and that `PrivateIPC=` has no effect on POSIX shared memory.
In a user manager, every unit runs as the same UID.

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
[v259](https://github.com/systemd/systemd/blob/v259/src/core/cgroup.c#L126)),
and `systemctl show` reports the configured value in both cases. Thus a loaded
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
  controller, but one file is absent, for example `memory.swap.max` when kernel
  swap accounting is off. From systemd sources, systemd treats a missing
  `memory.swap.max` as an expected case
  ([v252](https://github.com/systemd/systemd/blob/v252/src/core/cgroup.c#L397),
  [v255](https://github.com/systemd/systemd/blob/v255/src/core/cgroup.c#L438),
  [v259](https://github.com/systemd/systemd/blob/v259/src/core/cgroup.c#L403)).
  The setup error names the file and the kernel feature.

Both failures fail closed.

Swap accounting needs a check before setup accepts the unit, because setup adds
`MemorySwapMax=0` when the unit has `MemoryMax`. Setup reads `memory.swap.max`
in the root cgroup of the user manager. If the file is absent, setup rejects
the unit. The error names the automatic `MemorySwapMax=0` and tells the
operator to enable swap accounting or to remove `MemoryMax`.

## Unit limits

Limits belong to the owned unit, not to the profile. Adopted units get no
limits. To limit one, an operator converts it to an owned unit through setup
preview and apply. Trainers come first, then inference units.

Profiles that share one owned Ollama unit each carry a copy of the owned launch
specification (`nativeModel.owned`). The catalog validator requires equal
specifications for a shared unit. The `limits` type must be a comparable value
struct with no pointer, slice or map field. Then the existing equality check
and catalog clone stay correct. Tests must cover two shared profiles with equal
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

`MemorySwapMax` needs `MemoryMax` in this design, a deliberate narrowing of the
issue. The value struct cannot show the difference between an absent value and
0, so this rule gives both the same rendering.

### I/O devices

From systemd sources, systemd follows `slaves` to the originating device of a
device-mapper stack
([v252](https://github.com/systemd/systemd/blob/v252/src/core/cgroup.c#L855),
[v255](https://github.com/systemd/systemd/blob/v255/src/core/cgroup.c#L1101),
[v259](https://github.com/systemd/systemd/blob/v259/src/core/cgroup.c#L1034)).
It leaves a node that already has a request queue as it is. If the lookup
fails, systemd skips the limit with a warning
([v252](https://github.com/systemd/systemd/blob/v252/src/core/cgroup.c#L1169),
[v255](https://github.com/systemd/systemd/blob/v255/src/core/cgroup.c#L1417),
[v259](https://github.com/systemd/systemd/blob/v259/src/core/cgroup.c#L1265)).

Setup accepts only a leaf block device. The operator selects the disk. Setup
resolves it to its major and minor numbers and requires `S_ISBLK`, a `queue`
directory, no `partition` file, and an empty `slaves` directory under
`/sys/dev/block/MAJ:MIN`. Setup rejects the device if it cannot read `slaves`
or if that directory has entries. These entries identify backing devices in
device-mapper, md, bcache, and multipath stacks.

Setup reads the kernel name from `DEVNAME` in the `uevent` file under
`/sys/dev/block/MAJ:MIN`.

Host qualification must show that the device throttles a write. A matching
`io.max` line is not enough.

The catalog stores the canonical path `/dev/<kernel name>` and the numbers.
The renderer writes that canonical path, so systemd and the checks resolve the
same node. Each launch checks that the path is a root-owned block device node
with the recorded numbers. The launch parser compares the device token byte for
byte with the catalog value.

systemd writes all four keys on the device line, with `max` for a key that the
unit does not set. The readback requires the exact line. An example for device
259:0 with only a read bandwidth cap:

```text
259:0 rbps=524288000 wbps=max riops=max wiops=max
```

`IOWeight` has an effect only when the device uses the BFQ scheduler or the
iocost controller. The kernel guide names these two as the providers of weight
distribution. Setup reads `/sys/dev/block/MAJ:MIN/queue/scheduler` for `[bfq]`
and the root `io.cost.qos` line for `enable=1`. Setup reports which one
applies, or rejects `IOWeight` when neither applies.

With BFQ, systemd additionally writes `io.bfq.weight` with a value in the range 1 to
1000
([v252](https://github.com/systemd/systemd/blob/v252/src/core/cgroup.c#L1073),
[v255](https://github.com/systemd/systemd/blob/v255/src/core/cgroup.c#L1321),
[v259](https://github.com/systemd/systemd/blob/v259/src/core/cgroup.c#L1190)).
From systemd sources, the value B is N for N up to 100. Above 100, it is
`100 + (N - 100) * 900 / 9900` in integer arithmetic
([v252](https://github.com/systemd/systemd/blob/v252/src/basic/cgroup-util.h#L138),
[v255](https://github.com/systemd/systemd/blob/v255/src/basic/cgroup-util.h#L139),
[v259](https://github.com/systemd/systemd/blob/v259/src/basic/cgroup-util.h#L107)).
With BFQ, the readback requires both files. With iocost, it requires
`io.weight` only.

## Rendering and grammar

A unit with limits always renders `OOMPolicy=stop`. The default comes from
`DefaultOOMPolicy=`, which is `stop`, but the user manager reads it from its
own configuration
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
`parseOwnedLaunchUnit` for owned inference units, and the exact trainer form
for trainers. The parser removes the exact rendered block before the shared
grammar reads the rest, as it does for owned dependency lines. The owned-unit
path rejects a resource control in any other position, including the legacy
and `Startup` forms. The adopted and imported launch grammar does not change.
It accepts these directives as before, because those units get no limits from
the supervisor. The feature uses no drop-ins on owned units.

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
processes, including a trainer in the `exited` state with remaining processes.
The supervisor must record one successful populated readback for the invocation
before it binds `completed`. If the first observation finds an empty or absent
cgroup, the job stays pending. Explicit `training-recover` then records
`interrupted` when the job does not have a cause yet. The supervisor ignores a
cgroup that disappears during a later read only after that first sample, or
when a new read shows a different invocation.

The readback has two failure types. A value mismatch is drift. A read error or
a missing file is a readback failure. After a readback failure, the supervisor
reads `ActiveState`, `SubState`, `InvocationID` and `MainPID` again. The
failure counts when that second read shows the same invocation still active or
exited with a populated cgroup. It also counts when the second read itself
fails.

A counted failure stops the unit and returns a typed error. The supervisor does
not record a successful launch. The error names drift or readback failure, plus
the controller, the path, and the errno. The supervisor stores that cause on
the job. A later reconcile or `training-recover` keeps it and does not replace
it with a bare `interrupted`.

A unit test covers a cgroup that goes away between the state check and the
read.

Drift and a counted readback failure stop the unit, and the error names the
type. The results are:

- **Trainer.** The command that finds the failure stops the unit and does not
  retry the launch. That command is `training-start` after start or
  `training-reconcile` later. Reconcile does not bind `completed`, an exception to reconcile only
  reading evidence. A stored cause stays on the job for `training-recover`.
  Drift never gives `completed`. The implementation updates `docs/TRAINING.md`
  to match.
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
evidence contracts only. The event counters are diagnostics, never completion
or release evidence. A counter also goes away when systemd removes the empty
cgroup.

## Trainers

- A throttled trainer stays active. Reconcile reports that the trainer did not
  exit. Throttling never gives completion.
- A fenced `oom-kill` result is terminal `failed` evidence, as are
  `exit-code`, `signal` and `core-dump`. A task-creation or allocation failure
  normally becomes one of these results.
- `OOMPolicy=stop` also covers the `RemainAfterExit=yes` exited state. An OOM
  kill of a remaining process after the adapter exits still gives
  `Result=oom-kill`. If reconcile did not bind an outcome before, the systemd
  failure wins over a `completed` terminal file.
- A limit kill is a job outcome, not an error latch. After release
  verification, admission returns to idle under the existing training rules.

## Inference services

Inference keeps its existing health, transition and recovery rules. A limit
kill stops the owned unit, and the health check then fails. A throttle can
delay readiness or a response. The existing health deadlines and proxy terminal
evidence apply without change. Inference adds no new state, phase or result.

## Memory sizing for mapped models

llama.cpp maps the GGUF file of a model into memory. The kernel guide states
that the kernel charges a memory area to the cgroup that instantiated it. Thus
the kernel charges the page cache of a model to the unit that reads the model
first. A `MemoryHigh` below the model size plus the working set causes reclaim
and refaults during the load. The load can then exceed the readiness deadline
without a kill.

Set `MemoryHigh` above the model file size plus the measured working set, and
set `MemoryMax` above `MemoryHigh`. Measure both on the host during
qualification.

## Checks before admission release

A limit kill is not release evidence. After any termination, release stays on
the existing contract for that unit. Capacity is not an inference release
check.

Trainer release needs all of these, in order. The unit is `inactive` and
`dead`. The recursive cgroup is empty or absent. No GPU compute process stays
on the trainer GPU. Capacity is then available for the next admission.

Inference release needs `inactive` and `dead`, then `cgroup.events` `populated`
0 or an absent cgroup. It does not query compute apps. A shared owned Ollama
unit can stay `active/running` with an empty loaded-model list. That is the
existing idle release for that unit. A limit kill of that daemon still has to
reach `inactive/dead` before release.

A process in an uninterruptible driver wait can keep the cgroup populated after
the kill. Admission then stays closed. Failed cleanup stays fail-closed under
the existing recovery rules.

## Lifecycle

`Restart=no` stays. The feature adds no `StartLimit*` policy, no automatic
restart and no lifecycle phase. Idle release stops non-shared units. A shared
owned Ollama daemon can stay `active/running` with an empty model list, its
existing idle state. Before setup renders or reloads limit lines, that daemon
must be `inactive/dead`. `daemon-reload` would otherwise apply the new cgroup
values to the live daemon.

## Desktop

The setup application shows limits on the existing **Resource checks** page of
**Advanced settings**. Limits are off by default for each unit. When the user
enables limits, setup fills defaults for every host-cap control except
`MemoryHigh` and `MemoryMax`, which the operator sets. The page shows the
memory ceiling and the current slice usage next to the fields.

The page shows memory in GiB, CPU in cores and tasks as a count. I/O controls
and `IOWeight` are in a separate, closed section.

The page shows one host cap status:

| Status | Text | Action |
| --- | --- | --- |
| Present | Host protection is active for this account | None |
| Absent | This account has no host protection. An administrator must run one step as root | Show the exact drop-ins and commands, with a copy button |
| Different | Host protection settings do not match. An administrator must update them | Show each different value and the exact step |
| Escape | Programs from this account can run outside host protection | Name each route. Do not accept limits |

Status Present requires the hard slice ceilings and a passing escape check. An
open escape route is not Present.

Each fail-closed error class has plain-language text:

| Error class | Text |
| --- | --- |
| Controller not enabled | This computer does not let the account limit `<controller>`. An administrator must change the account settings |
| Interface file missing | The system cannot apply `<control>`. `<kernel feature>` is off on this computer |
| Host cap absent or different | The host protection of this account is missing or different. See the step on this page |
| Reserve violation | This limit leaves too little room for the supervisor. Use `<value>` or less |
| Escape route | Programs from this account can run outside host protection through `<route>`. Setup did not save limits |
| Memory pressure | Slice memory does not fit this limit. `<usage>` MiB is in use, and the ceiling is `<ceiling>` MiB |
| Memory usage unknown | Setup cannot read the memory already in use. Setup did not save limits |
| Drift | A limit changed after start. The supervisor stopped the workload. `<control>` was `<actual>`, not `<expected>` |
| Readback failure | Setup cannot read `<path>` (`<errno>`). The supervisor stopped the workload |
| `IOWeight` not applicable | The selected disk does not support I/O weight |

## Schema

- Catalog version 5 accepts all content of version 4, which includes the
  version 2 owned launches. It adds `limits` on the owned launch specification
  (`nativeModel.owned`) and on the trainer binding.
- In version 5, a trainer binding with limits uses binding version 2. A trainer
  binding without limits keeps binding version 1 and its precise bytes. Version 4
  accepts only binding version 1. The job file and terminal file versions stay 1.
- `limits` is strict JSON with bounded values. It has a `version` field and
  needs at least one control. The decoder rejects unknown fields and an empty
  or all-zero `limits` object, so an absent object is the only legacy form.
- Setup writes version 5 only when at least one unit has limits. Otherwise it
  keeps the exact legacy bytes and fingerprints.
- Limits pass through render, spec, fingerprint and rollback verification.
- The catalog version table in `docs/WORKLOADS.md` gets a version 5 row.

Every catalog reader must accept version 5:

- `Validate` of `control.Catalog` accepts version 5. Its gate for trainer
  bindings accepts version 4 and version 5.
- The proxy reads the catalog without `Validate`. Its `supportedCatalogVersion`
  guard must accept version 5. A test must send a version 5 catalog through the
  proxy.
- `Validate` rejects `limits` unless the catalog version is 5, and rejects
  trainer binding version 2 unless the catalog version is 5.
- The store guard for version changes already permits a change to version 5.
  It still rejects a change from version 5 to a lower version while `limits`
  remain. An older binary is safe only when the blob has no `limits` key. A
  lower version number is not enough, because that binary rejects an unknown
  field at version 2 or 4.

Every catalog writer uses one version rule:

- One Go function computes the lowest version that a catalog needs. The result
  is 5 with any limits, 4 with a trainer binding, 2 with an owned launch, and 1
  otherwise. Setup preview and apply both use it. While setup resumes a
  recorded activation, it keeps the recorded catalog bytes, including the
  version. It lowers the version only in a later write, after maintenance is
  clear.
- The function replaces the client rule in `clients/setup/setup-window.mjs`,
  which only raises the version, and the literal version 2 in
  `internal/setup/owned.go`.
- A draft with no `limits` stays draft version 1, and a new binary still reads
  it. A draft with `limits` uses the next draft version. Setup rejects a
  version 1 body that contains `limits` before unmarshal, so unmarshal cannot
  drop the field. The client `save-drafts` path uses the same rule.
  `ReadDrafts` today uses `json.Unmarshal`, which would drop the field.
- The test of version 2 to version 5 and back to the same version 2 bytes uses
  this function.

Older binaries reject version 5. To roll back, an operator first removes the
limits through setup and confirms that the catalog version went down. Only then
does the operator install the older binary. A backup restore is the other path.
The implementation adds these steps to `docs/RESTORING.md`.

When the catalog before the limits also used its lowest version, the result has
the exact earlier bytes. A catalog with a higher version than it needs gets the
lower version at the next setup write after maintenance is clear. Its unit
bytes and fingerprints stay the same. The write does not occur while an
activation resume is open.

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

Run each probe as the service UID on hosts with systemd 252, 255 and 259.
Install `stress-ng` from the distribution package first. Use the cgroup of each
probe unit, which `systemctl --user show -P ControlGroup` gives. Record the
systemd, kernel and `stress-ng` versions and the distribution in the private
host evidence, not in this record. Read each cgroup file while the probe unit
is active. Stop and reset each probe unit after the check.

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
| I/O cap holds | On the selected leaf disk, write a file larger than the read cap | The device throttles the write. An `io.max` line alone does not pass |
| BFQ weight | On a BFQ device: `systemd-run --user --unit=gws-q-bfq -p IOWeight=1000 sleep 600` | `io.weight` `default 1000` and `io.bfq.weight` `default 181` |

Each test category of #584 maps to one level:

| Test category | Level |
| --- | --- |
| Rendered values, fingerprint stability, legacy bytes, rollback bytes | Unit tests with golden files |
| Loaded values, drift, missing controllers, missing host cap, known drop-ins | Unit tests with a fake command runner and a fixture cgroup tree, then host qualification |
| Throttling without false completion, terminal failure | Unit tests with recorded unit states, then the throttle and OOM probes above |
| Cleanup and recovery failure | Unit tests with a fixture cgroup tree that stays populated |
| Applied cgroup values, OOM kill, host cap ownership | Host qualification only. These need a real user manager |
