# Resource limits: unimplemented design

[Documentation](README.md). See [implemented integration](INTEGRATION.md).

**Design for [#584](https://github.com/mickey-kras/gpu-workload-supervisor/issues/584). Not in the current release.** The release renders no resource-control directives. `requiredMiB` estimates launch VRAM demand. It enforces no ceiling. GPU VRAM enforcement and separate-account workload isolation are out of scope.

- [Host protection](#host-cap)
- [Unit limits](#unit-limits)
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

Setup cannot prove absence of all escapes. These checks fail closed. Errors name each matching file, spool, job, rule, unit, group or socket, or identify an unreadable policy.

| Route | Check |
| --- | --- |
| `cron` | Spools, system crontabs, and cronie allow or deny rules, as below |
| `at` | The same allow or deny rule, plus queued jobs, as below |
| Groups and sockets | Membership in `docker`, `podman`, `libvirt`, `lxd`, `incus`, `sudo`, `wheel` or `disk`, or a daemon socket this UID can open |
| `sudo` | Any rule from `sudo -l -U`, or a policy setup cannot read |
| `polkit` | A rule that lets this UID start a system unit, or a policy setup cannot read |
| System units | A loaded system unit with `User=` set to the service UID |

Scan `/var/spool/cron`, `/var/spool/cron/crontabs`, `/etc/crontab` and `/etc/cron.d` for UID jobs, by name or number, even without a cron binary. No user spool or system entry can remain. Also scan queued `at` jobs without relying on an installed binary.

Allow and deny files and parent directories need root ownership, no group or other write or UID-write ACL. Walk with `O_NOFOLLOW`. Files must be regular.

Unreadable `cron.allow` counts as absent. Only an exact username followed by newline counts: no trailing space or CR. Pass only with readable `cron.allow` omitting the user, or only `cron.deny` containing it.

Apply the same rule to `at.allow` and `at.deny`. Inspect the socket modes and ACLs, not just group membership.

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

Accept only the two exact host-cap drop-in names above and vendor files under `/usr/lib/systemd/system` or `/lib/systemd/system`. The package manager must identify unchanged systemd-package files. Root must own each file and ancestor without group or other write.

Unknown `DropInPaths` fail with named paths. Name ordering makes `50-` replace vendor `10-` values. Effective properties and cgroup readback remain decisive.

Unit tests need vendor fixtures and a fake package manager.

### Host cap checks

Before accepting or launching limited units, setup and preflight must check:

1. Root-owned regular host-cap files and real parent directories through `/etc`, walked with `O_NOFOLLOW`. No symlinks, group or other write, UID-write ACLs or capabilities.
2. Known `DropInPaths`, expected limit properties and `NeedDaemonReload=no` for slice and user manager.
3. Slice path from system-manager `ControlGroup` only. Root-owned limit files without group or other write or UID ACLs. Manager and unit cgroups descend from the slice. `memory.max`, `pids.max` and `cpu.max` are finite. `memory.swap.max=0`. When swap is off and accounting is impossible, the host-cap check accepts a missing swap file.
4. User-manager root `cgroup.controllers` includes `io` for I/O controls.
5. Every escape check passes.

Missing or different evidence fails closed with the drop-in path and differing property or file. Unlimited profiles need no cap. Setup displays exact drop-ins and selected values.

### Reserve and pressure

The shared cap includes supervisor, proxy and all UID workloads. Both unit memory limits must fit below slice `MemoryHigh` minus reserve. Other controls fit below their matching cap minus reserve.

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

Failed reads never mean 0. Before acceptance and admission, check `slice memory.current + unit MemoryMax + reserve <= slice MemoryHigh`. Failure names usage, ceiling and common `/dev/shm` and `tmpfs` causes.

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

- Controller absent from unit `cgroup.controllers`: delegation failure. Name controller and host-cap step.
- Controller present but interface file absent: name file and kernel feature. Systemd tolerates missing `memory.swap.max`. See source [12](#source-12). This design does not for limited units.

Before acceptance, check user-manager root `memory.swap.max`, because `MemoryMax` adds automatic `MemorySwapMax=0`. If absent, reject and instruct enabling swap accounting or removing `MemoryMax`.

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

For `IOWeight`, check `[bfq]` in device `queue/scheduler` or `enable=1` in root `io.cost.qos`. Report the provider. Reject if neither.

Iocost needs `io.weight` only. BFQ requires both weight files. Systemd writes BFQ range 1-1000. See source [16](#source-16).

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

Only `parseOwnedLaunchUnit` and the exact trainer form accept this block. Remove the exact rendered block before shared grammar parsing, as for owned dependencies. Adopted and imported grammar rejects limit directives. Use no owned-unit drop-ins.

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
| Escape | Programs from this account can run outside host protection | Name each route. Do not accept limits |

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

Present requires hard slice ceilings and passing escape checks. An open route cannot count as Present.

## Schema

Catalog v5 extends v4 (including v2 owned launches), adding `limits` to `nativeModel.owned` and trainer bindings. Limited trainers use binding v2. Unlimited trainers preserve binding v1 bytes.

Catalog v4 accepts binding v1 only. Job and terminal files stay v1. Strict bounded `limits` needs `version` and at least one control.

Unknown fields and empty or all-zero objects fail. Only absence is legacy. Include limits in render, spec, fingerprint and rollback checks and update the [version table](INTEGRATION.md#configured-workloads).

All readers must support v5: `control.Catalog.Validate` and proxy `supportedCatalogVersion` (proxy skips Validate). Test a v5 catalog through the proxy. Trainer gates accept catalogs 4 and 5.

Limits and trainer binding 2 need catalog 5. Store version guards already allow 5 but must reject lowering while limits remain. Older binaries reject the key even with catalogs 2 and 4.

One Go function selects minimum version: 5 with limits,4 with trainers,2 with owned launches,1 otherwise. Preview and apply share it. Replace the only-raise rule in `clients/setup/setup-window.mjs` and literal 2 in `internal/setup/owned.go`. Pending activation resumes exact recorded bytes and version. Lower the version only on later writes after maintenance clears.

Drafts without limits remain v1 and readable by new binaries. Limited drafts use the next version. Reject v1 bodies containing limits before unmarshal.

Current `ReadDrafts` uses `json.Unmarshal`, which would discard the field. Apply the same rule to client `save-drafts`. Test v2 to v5 and back to equal v2 bytes through the shared function.

Rollback: remove limits in setup, confirm lower catalog version, then install the older binary. Verified backup restore is the alternative. Add this to [restore instructions](OPERATIONS.md#make-a-backup-restore-and-roll-back-state).

If the original catalog used its minimum version, exact bytes return. Otherwise the next safe setup write lowers it without changing unit bytes and fingerprints. Never while activation resume remains open.

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
