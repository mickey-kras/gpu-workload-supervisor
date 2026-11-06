# Resource limits: design and schema

[Documentation](README.md). See [implemented integration](INTEGRATION.md).

**Design for [#584](https://github.com/mickey-kras/gpu-workload-supervisor/issues/584). This release has the schema, the runtime checks and the advisory escape route report. Setup cannot write limits yet.**

- Catalog v5 accepts `limits` on owned inference launches. The renderer and strict parser carry them.
- Escape routes are advisory warnings. Setup discovery reports them. They do not block limits, and no runtime check reads them. See [escape route report](#escape-route-report).
- For a target with limits, the supervisor checks the launch binding. Then it checks the controllers, the host cap, the reserve, the I/O device and the loaded unit properties. It does this before it stops the current workload. If a check fails, the supervisor refuses the switch and changes nothing. See [runtime checks](#runtime-checks).
- The start of the unit repeats these checks and also checks the slice memory. If a check fails, the unit does not start.
- Design decision: a unit with limits needs `memoryMaxMiB`, `tasksMax` and `cpuQuotaPercent` to start. Systemd applies them as `MemoryMax`, `TasksMax` and `CPUQuota`. It also needs each I/O cap whose key the slice `io.max` caps. When the slice `io.max` caps a device other than the unit `ioDevice`, the unit cannot cap that device. The runtime then refuses the unit with `ErrLimitReserve`. The schema accepts limits without them, so setup can read and remove them.
- Health checks repeat these checks, except the slice memory check, and read back the values in the cgroup. These checks change nothing.
- If a check after the start fails, the start stops the unit.
- In a switch, a later definite failure ends the verification, and the supervisor stops the unit. From a supervisor-owned state, the rollback stops it. From a user-owned state, the failed transition stops it before it latches the error state.
- A status check never stops a workload. It runs the health check only for a supervisor-owned, stable, healthy state with open admission. On a failed health check, it latches the error state and closes the admission.
- Design decision: nothing reads the limits back on a schedule. This release does not stop a running unit with limits after a failed status check. The unit keeps running behind the closed admission until `recover`. The limit stop of the policy tick and its escalation come with the setup change for limits. See [limit checks](OPERATIONS.md#limit-checks).
- Reconciliation keeps a running unit with limits and boot policy `retain` only after the checks pass. On a definite failure, it stops the unit and exits nonzero with the cause, so boot units that need a successful reconcile do not start. A definite failure in the readiness probe of a kept unit also stops it before the error state latches. The state keeps the workload that reconciliation kept, with error health and closed admission. The same write settles the state and latches the failure. Then run `recover` one time. It runs the checks again and keeps the unit when they pass. On a definite failure, it stops the unit and exits nonzero, without a second latch. A failure that proves nothing latches without a stop. If the unit does not run, `recover` settles the state and exits zero. Then fix the cause, or switch to Idle and remove the limits through setup.
- Each failure class has one result:

  | Failure | Class |
  | --- | --- |
  | A failed read of a limit file in the cgroup of a running unit | Definite. The runtime cannot prove the limit |
  | A missing trusted host cap file, device node, sysfs device entry or interface file | Definite |
  | A listed drop-in that is missing, while a second read still lists it with `NeedDaemonReload=no` | Definite |
  | A listed drop-in that is missing, while a second read does not list it or needs daemon-reload | Read failure. A package upgrade can remove a vendor drop-in |
  | A missing device scheduler file or `cgroup.controllers` at the user manager root | Read failure |
  | A slice property that differs from its file, while a second read shows other values or needs daemon-reload | Read failure. An administrator can change the cap during the check |
  | `NeedDaemonReload` other than `no` on the slice or the user manager | Read failure. A package upgrade can set it until the next daemon-reload. It proves no wrong cap |
  | A failed read of a binding file, such as the unit file, a drop-in, the model, the chat template or the API key file | Read failure |
  | A missing chat template or API key file | Definite |
  | A missing model, or a model that is not a directory (`ErrModelUnavailable`) | Read failure. It is not a limit refusal |
  | Other command and read failures, such as a failed read of a slice or host file | Read failure |

- A read failure before the unload fails the switch with no change and its usual code.
- In the start, a failure before the `systemctl start` call fails the switch with no stop. A failed start call or a failure after the call stops the unit and fails the switch.
- In the health checks after the start, a switch retries a read failure until its deadline.
- A status check or reconciliation keeps the admission closed without a stop.
- Setup cannot write limits. It rejects requests with limits before unit writes and accepts requests that remove them.

Later changes add trainer binding limits, draft and setup support and the **Resource checks** page. `requiredMiB` estimates launch VRAM demand. It enforces no ceiling. GPU VRAM enforcement and separate-account workload isolation are out of scope.

- [Host protection](#host-cap)
- [Unit limits](#unit-limits)
- [Runtime checks](#runtime-checks)
- [Drift and outcomes](#drift-checks)
- [Schema](#schema)
- [Qualification](#qualification-procedure)

## Method

Systemd claims below use the source tags v252, v255 and v259. Numbered source links follow this guide. Kernel claims use the unpinned [cgroup v2 guide](https://docs.kernel.org/admin-guide/cgroup-v2.html) and need host-kernel qualification.

No user manager was available during the spike, so no probe ran. The one offline systemd 255 parse check returned 0 without warnings. `MemoryMax=bogus` also returned 0, with a warning. Parsing is not validation: render validated values and compare loaded properties.

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

Root's `user-UID.slice` cap bounds even hostile processes inside the slice, without isolating workloads from each other. Service-UID unit limits protect against runaway workloads, not hostile same-UID code that can edit units and limits, reload the manager or stop the supervisor. This matches the existing [UID trust policy](../SECURITY.md).

The slice includes `user@UID.service` and UID session scopes from sshd and display managers. See source [1](#source-1). Delegation permits user writes to `cgroup.procs`, `cgroup.subtree_control`, `cgroup.threads`. V259 adds `memory.oom.group` and `memory.reclaim`. See source [2](#source-2). Systemd does not delegate the slice: root owns its limits, and the kernel prevents child escape above parent limits.

Limits protect against runaway workloads and accidents. They do not contain hostile code that runs as the service UID. Some routes let a program of the account run outside the slice. `gpu-setup discover` reports these routes as warnings. They do not block limits.

| Route | Warning |
| --- | --- |
| `cron` | A spool entry, a system crontab job or an allow or deny rule that lets the account use cron |
| `at` | A queued job or an allow or deny rule that lets the account use `at` |
| Groups and sockets | Membership in `docker`, `podman`, `libvirt`, `lxd`, `incus`, `sudo`, `wheel` or `disk`, or a daemon socket that the account can open |
| `polkit` | A rules file that mentions an action that starts system units, opens a host shell or runs a program as root |
| System units | A loaded system unit with `User=` set to the service account |

### Escape route report

`gpu-setup discover` returns the report as the `escapeRoutes` list. A later #584 change shows it on the desktop resource page. The supervisor and the runtime never run the scan.

Each entry has a `route` and an `item`. The route is one of `account`, `cron`, `at`, `groups`, `sockets`, `polkit`, `system-units`, `incomplete` and `more`. An empty list means that the scan found nothing. `null` means that no scan ran. The list has each entry only one time.

The list has at most 32 entries. The items about sources that the scan cannot check come first. When there are more findings, the last entry has the route `more` and gives their number and routes. When discovery stops the scan early, an entry with the route `incomplete` says that the scan cannot check the rest of the routes. A directory with more than 1024 entries gives "cannot check".

The scan runs as the desktop account, without root, and changes nothing. It runs no `sudo` or `pkcheck` command, because these commands ask for a password or write log entries for an account without rights. Membership in `sudo` or `wheel` is in the groups route.

| Route | Scan |
| --- | --- |
| Account | `/etc/passwd` gives the name and primary group of the account. Without an entry, or when the account cannot read the file, the scan reports that it cannot check cron, `at` and the member lists of `/etc/group` |
| `cron` | The allow and deny rule below. The spools `/var/spool/cron` and `/var/spool/cron/crontabs`: an entry with the account name or the account owner. The jobs of `/etc/crontab` and each file in `/etc/cron.d` with the account name or UID in the user field. The scan skips the directories, hidden files and backup files that end in `~` in `/etc/cron.d`, because cron does not run them |
| `at` | The allow and deny rule with `at.allow` and `at.deny`. Without both files, `at` lets only root in, so the rule gives no warning. The queues `/var/spool/at`, `/var/spool/atjobs` and `/var/spool/cron/atjobs`: an entry with the account name or the account owner |
| Groups | Membership through the primary group, the member list of `/etc/group` or a group of the setup process. `/etc/group` gives the group names also without a `/etc/passwd` entry |
| Sockets | The Docker, Podman, containerd, libvirt, LXD and Incus sockets. The scan follows a symlink to the socket. A socket that the account can reach is open when its mode lets the account write, or when it has an ACL. The scan ignores a socket path that the account cannot reach, and a path that is not a socket |
| `polkit` | Each `.rules` file in `/etc/polkit-1/rules.d`, `/run/polkit-1/rules.d`, `/usr/local/share/polkit-1/rules.d` and `/usr/share/polkit-1/rules.d` that mentions `org.freedesktop.systemd1.manage-unit`, `org.freedesktop.machine1` or `org.freedesktop.policykit.exec`. The rule can allow or deny the action, so the warning says only that the file mentions it |
| System units | `systemctl show --all` of the loaded system units, also inactive ones such as timer-activated services. `User=` with the account name or UID gives a warning. `user@UID.service` runs in the slice, so it gives none. The scan reads only property names and values, which `systemctl` does not translate. For a failed command, the report says that it cannot check the units. It adds the first line of the error and of the `systemctl` output |

The scan walks the cron, `at`, account and `polkit` files without symlinks. For a file or directory that the account cannot read, the scan reports "cannot check" and the path. It does the same for a symlink, a file where a directory must be and an allow or deny file that fails the trust checks.

Allow and deny files and their parent directories need root ownership, no group or other write, no ACL and no file capability. Files must be regular.

The report says that it cannot check an allow file that the account cannot read, because that file can list the account.

In a deny file, only the exact account name followed by a newline counts: no trailing space or carriage return. The rule denies the account only in two cases. First, a readable allow file omits the name. Second, no allow file exists, and a deny file lists the name. Without both files, Debian cron lets each user in, so cron gives a warning. An allow line with extra spaces or a carriage return counts as an allow.

On many hosts, the account cannot read the cron or `at` spools. Then the report says that it cannot check them. The `polkit` warning is best effort. The scan does not read `.policy` defaults or rules that match actions in another way. When the account cannot read a `polkit` rules directory, the report says that it cannot check it. The scan reads only local account files, not other account databases.

## Host cap

One manual root step installs two drop-ins: slice limits and user-manager I/O delegation. UID 1000 example:

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

Systemd recommends per-user slice drop-ins. See source [3](#source-3). `Delegate=io` adds to default `pids memory cpu`. See source [4](#source-4). Before installation or change, get verified Idle release and pause external service management. Then run `systemctl daemon-reload`.

Slice limits apply on reload. Changed delegation requires restarting `user@UID.service`, which stops supervisor and owned units. Without prior idle release, [boot reconciliation](OPERATIONS.md#boot-and-explicit-recovery) applies.

### Known drop-ins

All three versions ship `user-.slice.d/10-defaults.conf` (`TasksMax=33%`, `StopWhenUnneeded=yes`). See source [5](#source-5). They also ship ordering-only `user@.service.d/10-login-barrier.conf`. See source [6](#source-6). Distributions can add files.

Accept only the two exact host-cap drop-in names above and vendor files under `/usr/lib/systemd/system`. Accept vendor files under `/lib/systemd/system` only when `/lib` is a root-owned link to `usr/lib` or `/usr/lib`. The package manager must identify unchanged systemd-package files. Root must own each file and ancestor without group or other write.

Unknown `DropInPaths` fail with named paths. Name ordering makes `50-` replace vendor `10-` values. Effective properties and cgroup readback remain decisive.

Unit tests need vendor fixtures and a fake package manager.

### Host cap checks

Before accepting or launching limited units, setup and preflight must check:

1. Root-owned regular host-cap files and real parent directories through `/etc`, walked with `O_NOFOLLOW`. No symlinks, group or other write, UID-write ACLs or capabilities.
2. Known `DropInPaths`, expected limit properties and `NeedDaemonReload=no` for slice and user manager.
3. Slice path from system-manager `ControlGroup` only. Root-owned limit files without group or other write or UID ACLs. Manager and unit cgroups descend from the slice. `memory.max`, `pids.max` and `cpu.max` are finite. `memory.swap.max=0`. Each unit with limits needs `MemoryMax`, so the checks need swap accounting. A missing `memory.swap.max` at the root of the user manager is `ErrLimitInterface`. A missing slice `memory.swap.max` is `ErrHostCap`.
4. User-manager root `cgroup.controllers` includes `io` for I/O controls.

Missing or different evidence fails closed with the drop-in path and differing property or file. Unlimited profiles need no cap. Setup displays exact drop-ins and selected values.

### Reserve and pressure

The shared cap includes supervisor, proxy and all UID workloads. Both unit memory limits must fit below the lower of slice `MemoryHigh` and `MemoryMax`, minus the reserve. Other controls fit below their matching cap minus reserve.

| Control | Proposed reserve |
| --- | --- |
| `MemoryHigh`, `MemoryMax` | 2048 MiB |
| `MemorySwapMax` | 0 |
| `TasksMax` | 512 |
| `CPUQuota` | 100% |
| I/O bandwidth and IOPS caps | 10% of the host cap, rounded down |

Setup fills and displays defaults only for unset host-cap controls, except `MemoryHigh` and `MemoryMax`. The operator sets both memory limits from measured model size. Percent reserves round down to whole bytes and operations before subtraction. Defaults equal that maximum.

Reject out-of-schema defaults without writing limits. Never store 0 where value structs cannot distinguish absence. Use `MemoryMax > MemoryHigh`.

The example memory ceiling is 55296 MiB, not 59392 MiB from slice `MemoryMax`. Even unit `MemoryMax=55296` fails admission while the slice holds memory. Setup reads and displays actual `memory.current` beside the ceiling.

Failed reads never mean 0. Before acceptance and admission, check `slice memory in use + unit MemoryMax + reserve <= the lower of slice MemoryHigh and MemoryMax`. The memory in use is `memory.current` minus the reclaimable page cache: `active_file` plus `inactive_file` in the slice `memory.stat`. These lists leave out shmem and mlocked pages. A stopped model leaves its page cache in the slice, and the kernel can reclaim it. Failure names usage, page cache, ceiling and common `/dev/shm` and `tmpfs` causes.

Memory remains charged to its instantiating cgroup until release, even after unit stop. Do not divide the ceiling between units: each next unit must fit beside current charges. Size mapped models as below.

Render `OOMScoreAdjust=500` so slice OOM selects workloads before supervisor and proxy. `OOMPolicy=stop` keeps `memory.oom.group=0`. Only `kill` sets 1. See source [7](#source-7). Systemd stops remaining unit processes after the first kill.

Render neither `RemoveIPC=` nor `PrivateIPC=`: the former needs `User=`, `Group=` or `DynamicUser=`. The latter excludes POSIX shared memory. User-manager units share one UID.

## Controller checks

Default delegation is `pids memory cpu` (source [8](#source-8)). I/O requires its drop-in. Systemd reads root `cgroup.controllers` (source [9](#source-9)). It removes unsupported controllers from unit masks (source [10](#source-10)) and logs failed attribute writes (source [11](#source-11)). Units can start without enforcement while `systemctl show` still reports configured values.

| Controller | Controls |
| --- | --- |
| `memory` | `MemoryHigh`, `MemoryMax`, `MemorySwapMax` |
| `pids` | `TasksMax` |
| `cpu` | `CPUWeight`, `CPUQuota` |
| `io` | `IOReadBandwidthMax`, `IOWriteBandwidthMax`, `IOReadIOPSMax`, `IOWriteIOPSMax`, `IOWeight` |

Fail closed distinctly:

- A controller is not in `cgroup.controllers` at the root of the user manager: delegation failure. Name controller and the step for the delegation drop-in.
- Controller present at the user manager root but absent from the unit `cgroup.controllers`: systemd did not enable it for the unit. Name controller, `systemctl --user daemon-reload` and a new start of the workload through gpu-mode.
- Controller present but interface file absent: name file and kernel feature. Systemd tolerates missing `memory.swap.max`. See source [12](#source-12). This design does not for limited units.

Before acceptance, check user-manager root `memory.swap.max`, because `MemoryMax` adds automatic `MemorySwapMax=0`. If absent, reject and instruct enabling swap accounting. A unit with limits needs `MemoryMax`.

## Unit limits

Limits belong to owned units only. Adopted units need preview and apply conversion. Qualify trainers before inference.

Shared Ollama profiles copy `nativeModel.owned`. Catalog validation must enforce equal specifications, including limits. Use a comparable value struct without pointers, slices or maps so cloning and equality remain valid.

Test shared profiles with equal and unequal limits.

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

MiB values must align to page size. Systemd `M` uses 1024. CPU readback uses the default 100000 period. See source [13](#source-13).

`TasksMax` counts threads. Low values can prevent CUDA startup. `MemorySwapMax` requires `MemoryMax`. Absent and 0 intentionally render alike.

### I/O devices

Systemd follows device-mapper `slaves`, but retains nodes with request queues. See source [14](#source-14). Lookup failure skips limits with a warning. See source [15](#source-15). Accept only operator-selected leaf block devices: `S_ISBLK`, a `queue` directory, no `partition` file, and readable empty `slaves` under `/sys/dev/block/MAJ:MIN`. Nonempty slaves identify device-mapper, md, bcache or multipath stacks and fail.

Read `DEVNAME` from `uevent`. Store canonical `/dev/<kernel name>` and major and minor numbers. Render that path. At each launch, check root ownership and matching block-device numbers. Compare launch device tokens byte-for-byte. Host qualification must show write throttling, not merely an `io.max` line.

Readback requires the exact device line, including all four keys and `max` for unset caps:

```text
259:0 rbps=524288000 wbps=max riops=max wiops=max
```

For `IOWeight`, the kernel needs iocost. Then check `[bfq]` in device `queue/scheduler` or `enable=1` in root `io.cost.qos`. Report the provider. Reject if neither.

Iocost needs `io.weight` only. BFQ requires both weight files. Only iocost gives the `io.weight` file, so the check before unload requires `io.weight` at the user manager root. On a kernel without iocost, a unit with `IOWeight` does not start. Systemd writes BFQ range 1-1000. See source [16](#source-16).

For input N<=100, B=N. Otherwise `B=100+(N-100)*900/9900` with integer arithmetic. See source [17](#source-17).

## Rendering and grammar

Explicit `OOMPolicy=stop` avoids user-manager `DefaultOOMPolicy=` overrides (default stop). See source [18](#source-18). After `Restart=no`, render the fixed OOM lines and only configured controls in this order:

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

Only `parseOwnedLaunchUnit` and the exact trainer form accept this block. Remove the exact rendered block before shared grammar parsing, as for owned dependencies. The owned-unit path rejects resource controls in any other position, including legacy and `Startup` forms. Adopted and imported grammar still accepts these directives, because the supervisor gives those units no limits. Use no owned-unit drop-ins.

## Runtime checks

The runtime applies these checks to units with limits. Units without limits do not change. Where this design differs from these checks, the checks describe the current release. No check reads the escape routes.

| Time | Check | Error |
| --- | --- | --- |
| Before unload, before start | `cgroup.controllers` of the user manager root lists each required controller. When the memory controller is in use, `memory.swap.max` is present there. With `IOWeight`, `io.weight` is present there | `ErrLimitController`, `ErrLimitInterface`, `ErrLimitReadback` |
| Before unload, before start | Host cap. See the [host cap rules](#host-cap-rules) | `ErrHostCap`, `ErrLimitReserve`, `ErrLimitReadback` |
| Before unload, before start | Each unit limit is at most its slice cap minus the [reserve](#reserve-and-pressure). The memory cap is the lower of slice `memory.high` and `memory.max` | `ErrLimitReserve` |
| Before unload, before start | The I/O device is a root-owned leaf block device with the catalog number. With `IOWeight`, the device uses the active BFQ scheduler, or the root `io.cost.qos` line of the device has `enable=1`. Without BFQ, the root `io.cost.qos` is root-owned without group or other write, ACL or file capability | `ErrLimitDevice`, `ErrHostCap`, `ErrLimitReadback` |
| Before unload, before start | Loaded properties match the catalog, with `OOMPolicy` and `OOMScoreAdjust`. The binding check requires no drop-ins and `NeedDaemonReload=no` | `ErrLimitProperties`, `ErrLaunchChanged`, `ErrLaunchUnsupported` |
| Before start | The slice memory in use is `memory.current` minus the page cache (`active_file` plus `inactive_file` in `memory.stat`). That memory plus unit `MemoryMax` plus 2048 MiB is at most the lower of slice `memory.high` and `memory.max` | `ErrLimitMemory`, `ErrLimitReadback` |
| After start | The controller, host cap, reserve, device and loaded property checks again. Then the readback, as in the next row | `ErrLimitController`, `ErrLimitInterface`, `ErrHostCap`, `ErrLimitReserve`, `ErrLimitDevice`, `ErrLimitProperties`, `ErrLimitReadback` and `ErrLimitDrift`. A definite failure also carries `ErrLimitCheckFailed` |
| Each health check, before reconciliation keeps a unit | The binding and the checks before unload again. Then the readback: the memory, pids and cpu files, unset controls included. With a CPU quota, `cpu.max.burst` is 0, or absent on a kernel without burst. `io.max` only with an I/O cap. `io.weight` only with `IOWeight`, and `io.bfq.weight` with BFQ | The errors of the rows above, and `ErrLimitDrift`. A definite failure also carries `ErrLimitCheckFailed` |

The catalog schema accepts limits without `memoryMaxMiB`, `tasksMax`, `cpuQuotaPercent` or the I/O caps that the slice caps, so setup can read and remove them. The launch checks refuse such a unit with `ErrLimitReserve`. The error names the missing memory, task and CPU fields first. After you set them, it names each missing I/O cap.

A refusal before unload maps to `incompatible_configuration`. A definite failure after start carries `ErrLimitCheckFailed`. In a switch, the supervisor then ends the verification at once and stops the unit. From a supervisor-owned state, the rollback stops it. From a user-owned state, the failed transition stops it as a journaled limit stop before it latches the error state.

A readback failure of a unit that stopped during the readback is not definite. In the health checks after the start, a switch retries it until its deadline. In Start, each failure after the start call stops the unit. A failed `systemctl start` call also stops the unit, because systemd can still run the start job.

The stop after a failed check has a fixed limit of 2 minutes. This limit covers the SIGTERM phase of the systemd default stop timeout of 90 seconds and a prompt SIGKILL. Systemd gives each kill phase the same timeout. A process that stays in an uninterruptible wait after SIGKILL, such as a GPU driver wait, can make this stop fail first. Owned units do not set `TimeoutStopSec`, so a `DefaultTimeoutStopSec` of the user manager above about 110 seconds can also make it fail first. Then the start returns the stop error with the limit error, and systemd continues to stop the unit.

### Host cap rules

- `DropInPaths` of the slice lists the slice drop-in.
- With an I/O control, `DropInPaths` of the user manager lists the `Delegate=io` drop-in.
- `DropInPaths` lists only these drop-ins and vendor files under `/usr/lib/systemd/system`. The runtime accepts a vendor path under `/lib/systemd/system` only when `/lib` is a root-owned link to `usr/lib` or `/usr/lib`.
- The runtime walks each listed file without symlinks. Each file and each parent directory needs a root owner, no group or other write, no ACL and no file capability.
- Both units show `NeedDaemonReload=no`. Another value is `ErrLimitReadback`, not `ErrHostCap`. A launch fails, and the start stops the unit that it just started. An already running unit (status, reconcile) does not stop.
- The slice `ControlGroup` contains the user manager cgroup.
- The slice `cpu.max` gives a whole number of microseconds per second. The runtime does not support a non-default `CPUQuotaPeriodSec` on the slice, and the error says to remove it.
- The slice files `memory.high`, `memory.max`, `pids.max` and `cpu.max` are finite and root-owned without group or other write, ACL or file capability. The same trust rules apply to the slice `memory.swap.max` and `io.max`.
- With `IOWeight` and no BFQ, the root `io.cost.qos` is root-owned without group or other write, ACL or file capability. An untrusted file is `ErrHostCap`.
- The slice `memory.swap.max` is 0. A missing file is `ErrHostCap`. See [host-cap checks](#host-cap-checks).
- The slice properties match these files. On a mismatch, the runtime reads the properties again. Changed values or a reload need are `ErrLimitReadback`. The kernel keeps `memory.high` and `memory.max` in whole pages. The runtime rounds the systemd byte values down to a whole page before it compares them.
- With an I/O cap, the slice `io.max` has a finite cap of the same key for the device.
- The slice `io.max` caps no device other than the unit `ioDevice`. This rule applies to each unit with limits, also to a unit with no I/O fields. A unit has one `ioDevice`, so it cannot cap a second device. A cap on another device refuses the unit with `ErrLimitReserve`. To fix it, remove that cap from the slice drop-in. Or set `ioDevice` to that device and set its I/O caps.
- Each error of a unit state or drop-in names the unit: the slice or `user@UID.service`.
- A missing or untrusted host file is `ErrHostCap`. Other open and read failures are `ErrLimitReadback`. For `/lib`, a missing link, a `/lib` that is not a link, a link elsewhere or another owner is `ErrHostCap`.
- Some files have other classes. A missing root `io.cost.qos` means no iocost, which is `ErrLimitDevice` for a unit with `IOWeight` and no BFQ. A missing slice `io.max` passes when the unit has no I/O cap. A slice or hierarchy that is missing, or that the runtime cannot check or open, is `ErrLimitReadback`. A symlink or a file where a directory must be is `ErrHostCap`. The slice `memory.current` and `memory.stat` are usage counters, so any failed or untrusted read of them is `ErrLimitReadback`.

The runtime uses the proposed reserve values until the desktop options can change them. Package manager checks of vendor drop-ins come with setup support.

### Limits of the readback

The readback runs only at transitions, at health checks and at reconciliation. Nothing reads the values back on a schedule while the state is healthy. The unit cgroup is part of the user manager tree, which systemd delegates to the service account. The workload and any other program of that account can change the unit limits between two readbacks. The root-owned slice cap is the boundary that this account cannot change.

## Drift checks

All checks fail closed:

| Time | Required evidence |
| --- | --- |
| Setup acceptance and each launch | Required controllers in manager-root `cgroup.controllers`. Host-cap checks pass |
| Before admission | The memory-pressure check of the slice passes |
| Launch | Loaded properties match catalog, including OOM fields. Empty `DropInPaths`. `NeedDaemonReload=no` |
| After start and each reconcile | Exact cgroup readback while recursive processes remain, including exited trainers |

Before binding `completed`, record one populated readback for that invocation. An empty or absent first observation leaves the job pending. Explicit `training-recover` records `interrupted` only without an existing cause. Ignore later disappearance only after a successful sample or evidence of a different invocation.

Value mismatch means drift. Read error and missing file means readback failure. After read failure, reread `ActiveState`, `SubState`, `InvocationID`, `MainPID`.

Count failure if the same invocation remains active or exited with populated cgroup, or that reread fails. Stop the unit. Record no successful launch.

Typed errors name drift and readback failure, controller, path and errno. Preserve the job cause through reconcile and recovery rather than replacing it with `interrupted`. Test disappearance between state and file reads.

Trainer `training-start` or `training-reconcile` stops without retry. Reconcile must not bind completed, an exception to read-only reconciliation. Update [training](TRAINING.md) accordingly. Inference stop fails activation or stable state through existing health, transition and recovery rules.

## Outcome classes

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

See kernel `memory.high`, `memory.max`, `memory.events`, `pids.max`, `cpu.max`, `io.max`. With `OOMPolicy=stop`, OOM stops running or exited services as `oom-kill`. See source [19](#source-19). Counters are diagnostics, not completion and release evidence. They disappear with empty cgroups. Decisions use systemd state and existing evidence contracts.

### Trainers

Throttle leaves trainers active and incomplete. Fenced `oom-kill`, `exit-code`, `signal` and `core-dump` prove terminal failed. Allocation and task failures usually become these.

OOM after adapter exit still fails a `RemainAfterExit=yes` unit. Unless reconcile already bound an outcome, systemd failure overrides a completed terminal file. Limit kill is a job outcome, not error latch.

After verified release, admission returns to idle.

### Inference services

Keep existing health, transition and recovery rules: kill stops the unit and fails health. Throttle can delay readiness and response. Existing deadlines and terminal evidence apply. Add no state, phase or result.

### Memory sizing for mapped models

llama.cpp maps GGUF. First readers charge model page cache to their cgroup. Set `MemoryHigh` above model size plus measured working set, and `MemoryMax` higher. Otherwise reclaim and refaults can exceed readiness deadlines without kill. Measure on the host.

### Checks before admission release

Limit kill never proves release. Trainer release needs an `inactive/dead` unit, then an empty or absent recursive cgroup. Next, check for no compute process on its GPU, then capacity for admission. Inference release needs `inactive/dead` first. Its recursive cgroup must then be absent or show `cgroup.events` `populated 0`.

Capacity and compute-app queries are not inference release checks. Shared owned Ollama can idle `active/running` with no loaded models, but daemon kill must reach `inactive/dead`. Uninterruptible driver waits can keep processes after kill: keep admission closed and cleanup fail-closed.

### Lifecycle

Keep `Restart=no`. Add no `StartLimit*`, automatic restart or phase. Idle stops non-shared units. Stop the shared Ollama to `inactive/dead` before rendering and reloading limits: daemon-reload can otherwise alter its live cgroup.

## Desktop

Use Advanced settings' Resource checks page. Limits default Off per unit. Enablement applies the [defaults for unset controls](#reserve-and-pressure).

Display ceiling and current usage beside fields. Show memory in GiB, CPU in cores, tasks as count. Put I/O and weight in a closed section.

| Status | Text | Action |
| --- | --- | --- |
| Present | Host protection is active for this account | None |
| Absent | This account has no host protection. An administrator must run one step as root | Show the exact drop-ins and commands, with a copy button |
| Different | Host protection settings do not match. An administrator must update them | Show each different value and the exact step |

| Error class | Text |
| --- | --- |
| Controller not enabled | This computer does not let the account limit `<controller>`. An administrator must change the account settings |
| Interface file missing | The system cannot apply `<control>`. `<kernel feature>` is off on this computer |
| Host cap absent or different | The host protection of this account is missing or different. See the step on this page |
| Reserve violation | This limit leaves too little room for the supervisor. Use `<allowed>` or less, or remove the limits through setup. When no valid value fits, raise the slice cap to at least `<needed cap>`, or remove the limits through setup |
| Escape route (warning) | Programs from this account can run outside host protection through `<route>`. Limits protect against runaway workloads, not against such programs |
| Unchecked escape route (warning) | The scan `<item>`. A route can be open in a place that the scan cannot read |
| Escape routes left out (warning) | `<item>` are not on this page |
| Memory pressure | Slice memory does not fit this limit. `<usage>` MiB is in use. The `<cache>` MiB of page cache that the kernel can reclaim does not count. The start needs `<need>` MiB: `memoryMaxMiB` plus a 2048 MiB reserve. The ceiling is `<ceiling>` MiB. Check `/dev/shm` and `tmpfs` use |
| Memory usage unknown | Setup cannot read the memory already in use. Setup did not save limits |
| Drift | A limit changed after start. The supervisor stopped the workload. `<control>` was `<actual>`, not `<expected>` |
| Readback failure | Setup cannot read `<path>` (`<errno>`). The supervisor stopped the workload |
| `IOWeight` not applicable | `<number>` (`<device>`) does not support I/O weight (no BFQ or iocost). Without a device name in sysfs, the text gives the number only |

Present requires hard slice ceilings. Escape routes do not change the status. The desktop page of a later #584 change shows them as warnings beside it.

## Schema

Catalog v5 extends v4 (including v2 owned launches), adding `limits` to `nativeModel.owned` and trainer bindings. Limited trainers use binding v2. Unlimited trainers preserve binding v1 bytes.

Catalog v4 accepts binding v1 only. Job and terminal files stay v1. Strict bounded `limits` needs `version` and at least one control.

Unknown fields and empty or all-zero objects fail. Only absence is legacy. Include limits in render, spec, fingerprint and rollback checks and update the [version table](INTEGRATION.md#configured-workloads).

All readers must support v5: `control.Catalog.Validate` and proxy `supportedCatalogVersion` (proxy skips Validate). Test a v5 catalog through the proxy. Trainer gates accept catalogs 4 and 5.

Limits and trainer binding 2 need catalog 5. Store version guards already allow 5 but must reject lowering while limits remain. Older binaries reject the key even with catalogs 2 and 4.

One Go function selects minimum version: 5 with limits,4 with trainers,2 with owned launches,1 otherwise. Preview and apply share it. It replaces literal 2 in `internal/setup/owned.go`. Client mirror `clients/setup/review.mjs` replaces the only-raise rule in `clients/setup/setup-window.mjs`. The backend recomputes the version before preview and apply.

Pending activation resumes exact recorded bytes and version. Lower the version only on later writes after maintenance clears.

Drafts without limits remain v1 and readable by new binaries. Limited drafts use the next version. Reject v1 bodies containing limits before unmarshal.

Current `ReadDrafts` uses `json.Unmarshal`, which would discard the field. Apply the same rule to client `save-drafts`. Test v2 to v5 and back to equal v2 bytes through the shared function.

Rollback: remove limits in setup, confirm lower catalog version, then install the older binary. Verified backup restore is the alternative. See [roll back a release](OPERATIONS.md#roll-back-a-release).

If the original catalog used its minimum version, exact bytes return. Otherwise the next safe setup write lowers it without changing unit bytes and fingerprints. Never while activation resume remains open.

| Field | Range | Rule |
| --- | --- | --- |
| `memoryHighMiB` | 256 to 16777216 | Less than `memoryMaxMiB` when the unit has both |
| `memoryMaxMiB` | 256 to 16777216 | Required to start the unit |
| `memorySwapMaxMiB` | 0 to 16777216 | Needs `memoryMaxMiB`. 0 when absent |
| `tasksMax` | 64 to 4194304 | Required to start the unit |
| `cpuWeight` | 1 to 10000 | None |
| `cpuQuotaPercent` | 1 to 102400 | Required to start the unit |
| `ioDevice` | `^/dev/[a-z][a-z0-9]{0,31}$` | Required with any I/O field. Setup writes the canonical kernel name |
| `ioDeviceNumber` | `MAJ:MIN` | Required with `ioDevice`. Setup writes it |
| `ioReadBandwidthMax`, `ioWriteBandwidthMax` | 1048576 to 1099511627776 | Bytes per second. Required to start the unit when the slice `io.max` caps the key |
| `ioReadIOPSMax`, `ioWriteIOPSMax` | 100 to 10000000 | Operations per second. Required to start the unit when the slice `io.max` caps the key |
| `ioWeight` | 1 to 10000 | Needs iocost in the kernel, and BFQ or iocost on `ioDevice` |

## Qualification procedure

Run as service UID on systemd 252, 255 and 259 with distribution `stress-ng`. Get probe cgroups with `systemctl --user show -P ControlGroup`. Inspect the files while active, then stop and reset units. Keep systemd and kernel and stress-ng versions and distribution in private host evidence.

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

| Test category | Level |
| --- | --- |
| Rendered values, fingerprint stability, legacy bytes, rollback bytes | Unit tests with golden files |
| Loaded values, drift, missing controllers, missing host cap, known drop-ins | Unit tests with a fake command runner and a fixture cgroup tree, then host qualification |
| Throttling without false completion, terminal failure | Unit tests with recorded unit states, then the throttle and OOM probes above |
| Cleanup and recovery failure | Unit tests with a fixture cgroup tree that stays populated |
| Applied cgroup values, OOM kill, host cap ownership | Host qualification only. These need a real user manager |

## Source references

Numbered references identify the v252/v255/v259 source evidence used above:

<a id="source-1"></a>

1. [252](https://github.com/systemd/systemd/blob/v252/man/user%40.service.xml#L59) / [255](https://github.com/systemd/systemd/blob/v255/man/user%40.service.xml#L59) / [259](https://github.com/systemd/systemd/blob/v259/man/user%40.service.xml#L60)
<a id="source-2"></a>

2. [252](https://github.com/systemd/systemd/blob/v252/src/shared/cgroup-setup.c#L399) / [255](https://github.com/systemd/systemd/blob/v255/src/shared/cgroup-setup.c#L400) / [259](https://github.com/systemd/systemd/blob/v259/src/shared/cgroup-setup.c#L194)
<a id="source-3"></a>

3. [252](https://github.com/systemd/systemd/blob/v252/man/user%40.service.xml#L80) / [255](https://github.com/systemd/systemd/blob/v255/man/user%40.service.xml#L80) / [259](https://github.com/systemd/systemd/blob/v259/man/user%40.service.xml#L81)
<a id="source-4"></a>

4. [252](https://github.com/systemd/systemd/blob/v252/src/core/load-fragment.c#L3974) / [255](https://github.com/systemd/systemd/blob/v255/src/core/load-fragment.c#L4018) / [259](https://github.com/systemd/systemd/blob/v259/src/core/load-fragment.c#L3991)
<a id="source-5"></a>

5. [252](https://github.com/systemd/systemd/blob/v252/units/user-.slice.d/10-defaults.conf) / [255](https://github.com/systemd/systemd/blob/v255/units/user-.slice.d/10-defaults.conf) / [259](https://github.com/systemd/systemd/blob/v259/units/user-.slice.d/10-defaults.conf)
<a id="source-6"></a>

6. [252](https://github.com/systemd/systemd/blob/v252/units/user%40.service.d/10-login-barrier.conf) / [255](https://github.com/systemd/systemd/blob/v255/units/user%40.service.d/10-login-barrier.conf) / [259](https://github.com/systemd/systemd/blob/v259/units/user%40.service.d/10-login-barrier.conf)
<a id="source-7"></a>

7. [252](https://github.com/systemd/systemd/blob/v252/src/core/service.c#L778) / [255](https://github.com/systemd/systemd/blob/v255/src/core/service.c#L846) / [259](https://github.com/systemd/systemd/blob/v259/src/core/service.c#L909)
<a id="source-8"></a>

8. [252](https://github.com/systemd/systemd/blob/v252/units/user%40.service.in#L24) / [255](https://github.com/systemd/systemd/blob/v255/units/user%40.service.in#L24) / [259](https://github.com/systemd/systemd/blob/v259/units/user%40.service.in#L28)
<a id="source-9"></a>

9. [252](https://github.com/systemd/systemd/blob/v252/src/basic/cgroup-util.c#L1967) / [255](https://github.com/systemd/systemd/blob/v255/src/basic/cgroup-util.c#L2052) / [259](https://github.com/systemd/systemd/blob/v259/src/basic/cgroup-util.c#L1659)
<a id="source-10"></a>

10. [252](https://github.com/systemd/systemd/blob/v252/src/core/cgroup.c#L1919) / [255](https://github.com/systemd/systemd/blob/v255/src/core/cgroup.c#L2213) / [259](https://github.com/systemd/systemd/blob/v259/src/core/cgroup.c#L1852)
<a id="source-11"></a>

11. [252](https://github.com/systemd/systemd/blob/v252/src/core/cgroup.c#L109) / [255](https://github.com/systemd/systemd/blob/v255/src/core/cgroup.c#L115) / [259](https://github.com/systemd/systemd/blob/v259/src/core/cgroup.c#L126)
<a id="source-12"></a>

12. [252](https://github.com/systemd/systemd/blob/v252/src/core/cgroup.c#L397) / [255](https://github.com/systemd/systemd/blob/v255/src/core/cgroup.c#L438) / [259](https://github.com/systemd/systemd/blob/v259/src/core/cgroup.c#L403)
<a id="source-13"></a>

13. [252](https://github.com/systemd/systemd/blob/v252/src/core/cgroup.c#L968) / [255](https://github.com/systemd/systemd/blob/v255/src/core/cgroup.c#L1216) / [259](https://github.com/systemd/systemd/blob/v259/src/core/cgroup.c#L1146)
<a id="source-14"></a>

14. [252](https://github.com/systemd/systemd/blob/v252/src/core/cgroup.c#L855) / [255](https://github.com/systemd/systemd/blob/v255/src/core/cgroup.c#L1101) / [259](https://github.com/systemd/systemd/blob/v259/src/core/cgroup.c#L1034)
<a id="source-15"></a>

15. [252](https://github.com/systemd/systemd/blob/v252/src/core/cgroup.c#L1169) / [255](https://github.com/systemd/systemd/blob/v255/src/core/cgroup.c#L1417) / [259](https://github.com/systemd/systemd/blob/v259/src/core/cgroup.c#L1265)
<a id="source-16"></a>

16. [252](https://github.com/systemd/systemd/blob/v252/src/core/cgroup.c#L1073) / [255](https://github.com/systemd/systemd/blob/v255/src/core/cgroup.c#L1321) / [259](https://github.com/systemd/systemd/blob/v259/src/core/cgroup.c#L1190)
<a id="source-17"></a>

17. [252](https://github.com/systemd/systemd/blob/v252/src/basic/cgroup-util.h#L138) / [255](https://github.com/systemd/systemd/blob/v255/src/basic/cgroup-util.h#L139) / [259](https://github.com/systemd/systemd/blob/v259/src/basic/cgroup-util.h#L107)
<a id="source-18"></a>

18. [252](https://github.com/systemd/systemd/blob/v252/src/core/main.c#L662) / [255](https://github.com/systemd/systemd/blob/v255/src/core/main.c#L686) / [259](https://github.com/systemd/systemd/blob/v259/src/core/main.c#L811)
<a id="source-19"></a>

19. [252](https://github.com/systemd/systemd/blob/v252/src/core/service.c#L3415) / [255](https://github.com/systemd/systemd/blob/v255/src/core/service.c#L3648) / [259](https://github.com/systemd/systemd/blob/v259/src/core/service.c#L4049)
