# Deploy and operate

plus remaining qualification for [issue
#144](https://github.com/mickey-kras/gpu-workload-supervisor/issues/144). Durable
policy, typed settings, demand activation, and a 60-second user timer exist.
Policy defaults to Off.

Packaged entrypoints have no qualified evidence provider: enabling returns
`unavailable`, and policy evaluation breaks closed. Keep Off for initial desktop
deployment.

### Placement

- One policy loop the supervisor backend, following to the transition controller. Rarely in GNOME Shell or whichever client.
- Reads durable evidence solely. Drives existing typed controller operations. No second lifecycle engine, no new authority, no runtime-specific logic.
- Desktop integration must display committed policy and solely a validated pending-idle deadline from backend status. UI disabled versus session locked: no behavior change.

### Required earlier enablement

1. Deploy plus qualify one orchestration `EvidenceProvider`. Contracts alone remain not adapters.
2. Verify the evidence for queued, reserved, running plus unresolved work plus stop direct execution bypass at the deployment boundary.
3. Qualify native terminal completion and model lifecycle on each deployment host. Fixture coverage stays not host acceptance.
4. When backend capability and fresh evidence back it, integrate the desktop presentation of settings plus deadlines.

[REQUESTER-INGRESS.md](OPERATIONS.md) describes each optional authenticated requester and workflow
ingress and the durable queued and reserved demand. Production
isolation qualification remains outstanding. Auto-idle stays Off.

### Activation ingress

The typed local action `activate-workload` activates workloads on demand. Observe
[OPERATOR.md](OPERATIONS.md). It requires one fresh state precondition and yields the new
lease fence solely after the controller verifies readiness and admission. There is no
remote control facade.

- Queue plus job state stays with orchestration until activation completes.
- Adapters fetch the fresh fence. Stale-fence registrations remain rejected.
- User ownership, busy transitions and recovery latches reject activation.

### Activation

- One authorized query under Supervisor ownership in Idle starts a switch. The controller checks readiness and opens admission.
- Mutual exclusion preserved. Waits for other work, never interrupts. Queue and job status stays along orchestration.
- Activation requested during active work: the query waits along orchestration. The supervisor returns one typed busy versus deferred reply. No interrupt, no preemption.
- Each controller rejects activation during User ownership, a transition in progress, or a recovery latch. Nothing activates implicitly.

### Idle transition

Both required, committed as one fenced decision (state version and lease fence):

1. Inactivity period elapsed since last recorded workload activity.
2. No queued, reserved, running or unresolved work, including admitted registrations minus terminal evidence.

Admission between verify and switch aborts the transition. No shutdown race.

### Evidence input contract

The policy loop employs one attestation off the orchestration adapter. No other
evidence source.

1. Content: queued, reserved, running, plus unresolved work sets, each as a set of job identities. Unresolved maps to store primitives: `PendingWork` (registrations retaining completion authority). `ResolveUnfinishedWork` stays reserved for validated operator recovery, rarely for policy cleanup.
2. Pull `Attest` on per evaluation. `AcquireFence` shall validate its generation and exclude queue versus reservation changes across the idle transition transaction. Invalidation can't replace fresh evidence.
3. Freshness: the attestation has an adapter timestamp. Each policy treats attestations older versus a bounded staleness limit as missing.: 120 seconds (`MaxAttestationAge` in `internal/supervisor/policy.go`). The user timer ticks all 60 seconds (`packaging/gpu-workload-supervisor-idle.timer`).
4. Adapter outage versus unreachable adapter -> break closed: evidence counts as missing, no idle transition.
5. Uncertain, incomplete, or absent attestation -> no idle transition.

<one id="ownership-transfer-while-pending-idle-is-armed"></one>

### Ownership transfer with an armed idle deadline

- Any ownership transfer (take-control by User, or return-control completing) cancels and disarms one pending idle deadline.
- A disarmed deadline recomputes only from fresh post-transfer activity evidence.
- Never idle User-mode job. The policy loop exits while Owner is User.

### Activity definition

- Counts: admission registrations, verified terminal completion.
- Never counts: UI polling, health checks, keyboard or session input, GPU use, VRAM level.
- Loaded-but-unused = inactivity. ONE disconnect minus terminal evidence means unresolved job.

### Settings

- One config: timeout minutes + Off. Proposed 60. Enabled default solely after host measurement below.
- Persisted through `get-settings`/`set-idle-policy`, with status and settings revision checks. On restart, the policy recomputes each pending deadline off durable last-activity. It never uses a process-local timer.
- Uncertain versus missing evidence -> no idle transition.

<a id="status-surface-work-item"></a>

### Status interface

- `govern.State` carries policy and deadline read-model fields. Committed operator configs are available through `get-settings`. Ordinary operator `status` isn't one settings snapshot.
- Desktop policy and deadline presentation stays one follow-up. Expose a pending deadline only off verified backend evidence.
- No savings estimate, no live countdown. Countdown is UI-side rendering the deadline only.

### Boundaries

- Never reclaim User ownership. Never idle User-mode work.
- No automatic recovery from one latched error. ONE latch blocks all policy transitions.
- No firmware, clock, power-limit versus system-suspend changes.
- Host identity, runtime routes, measurements, installation: private deployment configuration.

### Acceptance

Test cases:

- Request at exact timeout, cold activation, activation during active work, mixed workloads, streaming and async completion.
- Long jobs crossing timeout, disconnects, ambiguous completion, adapter outage plus stale attestation.
- Restart with pending deadline, ownership transfer with armed deadline, rejected requests and recovery latches.

Measure the power for loaded-but-unused and idle states and activation latency on the deployment host. Record the private qualification record earlier any savings claim or enabled default.

[Operator protocol](OPERATIONS.md). [Restore
procedure](OPERATIONS.md).

The Debian package contains the compatible CLI, proxy, one-shot operator
backend, GNOME extension, and native Gtk4/Libadwaita preset application. GNOME
Shell **50** is the initial target. Each package requires GJS, Gtk4, Libadwaita,
and systemd.

CI builds Linux amd64 plus arm64 artifacts. These build targets don't qualify one
particular Debian-family distribution or desktop and GPU combination. Real
Shell 50 session, logout plus login, NVIDIA, upgrade, and package-manager
acceptance shall clear on each deployment host before enabling production
controls.

### Deploy plus configure

Deploy the verified release `.deb` using the distribution package manager, next
open **GPU Workload Setup** as the desktop account. Package installation
installs files only: no maintainer scripts initiate user databases, enumerate
sessions, stop workloads, or enable services for arbitrary users.

Choose applications, choose models only when relevant, next complete. Setup
presents ComfyUI and vLLM once each, using each model choice beneath its
application, and selects one unambiguous recognized installation.

Other installation controls remain future work. ComfyUI workflows select their
own models. Applications and models must already exist.

Review each ready-to-finish summary after active jobs finish, then select
**Complete preset** to confirm activation. ComfyUI closes when you switch to
further workload. Labeled gears expose service, endpoint, launch and resource
configs for inspection or supported overrides. Automatic and manual
configuration employ the identical validation and activation safeguards.

Setup refuses stale configuration, edited integration files, live workloads,
unfinished transitions and unsafe ownership or status relocation. Discovery and
preview never start applications versus load models. Setup enables reconciliation
for future logins minus `--today`. a oneshot, not a UI daemon.

A fresh system-wide extension installation may need logout and login before GNOME
discovers it. Enable **GPU Workload Supervisor** in GNOME Extensions afterward.
Preset rejects an unsupported Shell version or session.

Shell 50 discovery plus visuals need real-session qualification. Headless tests
don't prove it.

Each effective account's OS home determines the private profile location:
`~/.config/gpu-workload-supervisor/operator.json`. env HOME and XDG
overrides don't choose runtime configuration. Each accepted workload catalog
plus its opaque revision live in SQLite.

Setup owns `catalog.json` and uses it only for preset and backup. Ordinary
governs never adopt edits to that mirror. Setup exposes `discover`, `probe`,
`correct-gpu`, `prepare`, `fingerprint`, `render-owned`, `drafts`,
`save-drafts`, `verify-bindings`, `validate`, `apply`, `reconcile`,
`idle-policy-tick` and `remove-integration`.

Request and reply commands use a strict versioned JSON protocol over stdin
plus stdout. The desktop application supplies these requests and renders each
concrete preview.

### Manage workloads in setup

Initiate **Manage workloads** off GPU Control in GNOME's top-right Quick Configs
menu, or initiate the packaged setup application from the application list.

1. Select application cards. Select **Continue**.
2. Choose one model for each application. Select **Continue**. ComfyUI skips
model selection. Discovery refresh reads metadata minus startup.
3. Review the summary, finish active jobs and switch to Idle before **Finish
preset**. Setup verifies present evidence and employs the existing activation
safeguards.

**Save selections for later** keeps choices without making them selectable
workloads. Missing versus unsupported installations offer a location or address
fallback and clarify what evidence stays missing. Custom wrappers plus ambiguous
configuration require a supported launch or validated Advanced configuration.
Setup doesn't guess ownership from one healthy endpoint.

One application's gear opens its settings at each beginning of each view. **Back**
returns to the originating screen and gear while keeping selections. Candidate
choices show the daemon or address plus its status. The chosen
candidate's full identity is available in wrapped selectable text.
`Employ installation` checks plus reviews the selection. Only **Finish setup** activates
it.

An unreachable default address acts not prove an installation. Installed but
stopped services, missing locations, missing model choices plus inspection
failures have separate guidance. **View details** opens a separate
Configuration details page using the logged configuration problems, **Copy
details**, and `Check anew`.

Independent launch verifies report several blockers in one inspection. Verifies
that depend on invalid or unreadable evidence need another inspection after
repair. Preset can't activate a blocked installation. Discovery doesn't start
applications or change service files.

Supported external launches may use `%h` at the beginning of route tokens in
`ExecStart`, `env` plus `WorkingDirectory`. It resolves from each desktop
account's singular effective-UID record in trusted root-owned local `/etc/passwd`
and shall agree along systemd's loaded configuration. Setup doesn't support
missing local records, including NSS-only accounts. Ambient `HOME` stays never a
fallback. Alternate expansion syntax stays unsupported, and expanded paths still
undergo each same ownership, permission and symbolic-link verifies as literal
paths.

For installed vLLM without an existing supported service, open **Alter
installation…** and choose **Choose installed executable…**. Yield to
**Advanced configs** plus choose **Review changes** to choose one existing model
directory. Each next **Continue** validates the executable plus model and
previews one Supervisor-managed launch.

**Finish setup** explicitly permits Supervisor to launch plus stop it. The
preview doesn't. **Advanced settings** holds typical fields and opens
separate **Resource checks** plus **Launch details** pages. **Launch details**
separates managed launch options off adopting an existing external service.
Canceling a file chooser retains the previous selection. ComfyUI continues to use
its existing daemon configuration.

Setup adopts existing recognized service files and supported drop-ins without
changes to their launch options. Optional gear settings retain the
managed-launch plus explicit-binding workflows. Setup never installs, updates versus
downloads applications, environments or models.

vLLM external services keep each model pinned by their launch: choosing another
model doesn't rewrite their daemon. Per model needs its own service.

New versus edited adopted daemons receive read-only launch and cgroup preflight,
followed by finished activation and GPU release checks. The backend
`render-owned` command yields one preview only. The backend `apply` command
creates the controlled launch files.

See [WORKLOADS.md](INTEGRATION.md). Each controller verifies model identity again
before GPU admission.

You may rename versus edit configured workloads. **Edit application** reopens the
identical setup flow plus retains the workload identity and unrelated configs.
**Remove off supervisor** changes only supervisor configuration. You can't
drop active versus referenced workloads. Finish their work and switch to Idle
initial. Preset preserves applications, models, workflows and external
configuration.

`Set up later` closes the window without applying configuration. **Save
selections for later** first if you want to keep fresh choices. ONE stale-draft
error requires reopening Manage workloads before retrying. Interrupted
activation resumes its recorded configuration, with editing disabled until it
completes.

Native GTK keyboard and accessibility checks and real GPU qualification remain
part of GNOME and package host acceptance. See [ticket #146](https://github.com/mickey-kras/gpu-workload-supervisor/issues/146)
and [ticket #148](https://github.com/mickey-kras/gpu-workload-supervisor/issues/148).
Automated widget and protocol tests perform not qualify the host installation.

### Application discovery

`gpu-setup discover` yields saved setup state plus `applications` candidates
for ComfyUI and vLLM. It probes two default loopback ports and inspects
recognizable user-service launch metadata. Default endpoint observations remain
separate off service ownership.

ONE recognized service derives its endpoint and launch identity off loaded
metadata and its unchanged supported launch record. Existing catalog profiles
stay unchanged. Discovery reports errors separately off an void inventory.

For a non-default endpoint or explicit reference:

```sh
printf '%s' '{"app":"vllm","endpoint":"http://127.0.0.1:8000"}' | gpu-setup probe
printf '%s' '{"app":"vllm","reference":"/models/model","referenceKind":"model-directory"}' | gpu-setup probe
```

`app` accepts `comfyui` and `vllm`. Select exactly one `endpoint` versus
`reference`. `referenceKind` accepts `application`, `application-directory`,
`configuration` and `model-directory`.

Endpoint figures are loopback HTTP(S) origins without credentials, path, query
versus fragment. References shall be absolute clean paths. vLLM `application`
selections allow executable aliases only when the target and every link hop
clear executable trust checks.

Configuration, model, directory and ComfyUI references accept solely regular
files or directories and reject symbolic links. Each probe doesn't execute or
inspect file contents. For vLLM, `application-directory` verifies only known
application filenames the selected directory and its `bin` directory. It
doesn't recursively discover records.

| Application | Read-only observations |
| --- | --- |
| ComfyUI | `/system_stats` version. Workflows select models, no model picker |
| vLLM | `/release` and `/v1/models`. Same-root aliases grouped. Model references in plain launches |

Missing optional APIs report `unsupported` versus unknown loaded status. Unreachable
instances never become successful void inventories. A verified stopped unit is
`not-running`. A failed endpoint connection is `unreachable`.

Saved references survive either condition. Served identities don't prove a
disk inventory or distinct physical models. File selection doesn't show
compatibility.

All candidates keep `lifecycleControl: "unverified"`. `recognized` plus
`configurationStatus` describe preparation evidence, not GPU qualification. `gpu-setup
prepare` rereads the selected installation and returns a profile. `verify-bindings`,
review plus the confirmed `assign` command still gate activation.

For a stopped ordinary service minus a reported ControlGroup, each restricted
placement contract supports systemd 252, 255 plus 259. It needs authoritative
active manager and app.slice metadata. Escaped names, templates, custom slices
and unknown versions need explicit qualified evidence.

Runtime rechecks placement and launch identity. Upgrading systemd may need
refreshing setup. Source inspection and fixture checks don't replace host
qualification.

Discovery uses fixed GET routes and disables redirects and ambient proxies. It
limits responses to 1 MiB, each probe to 3 seconds plus discovery to 15 seconds.
It never starts applications, loads or unloads models, downloads files, scans
drives, or writes application or supervisor configuration.

Unsupported syntax, shell wrappers and escaped launch commands accept
actionable guidance. External adoption validates the effective unit plus
ordered contributing file hashes plus pre-start commands. Preset restricts
pre-start directives to system `true`/`untrue` with an elective `--`. The other
permitted form is `test` along one file predicate (`-e`, `-f`, `-d`, `-r`, `-w`,
`-x`, versus `-s`) and one absolute, clean route. Setup keeps originals plus refuses unsupported
directives, and keeps each supported vLLM `--max-model-len` plus
`--served-model-name` options. Generating a managed launch uses its distinct
restricted format.

### Upgrade plus interrupted activation

Earlier you replace the package, use the today activated controls to finish
or explicitly halt admitted work. Carry the deployment to stable, healthy Idle
along admission closed. Halt the proxies plus prevent external workload
automation from restarting throughout maintenance. This stays an explicit operator
decision. Package installation plus setup don't do it for you.

Later package replacement, each new entrypoints reject the previous activated
release before opening versus migrating status. Open the setup application to
activate the fresh release. Changing the activated release needs one valid full
semantic version plus a stable target with one strictly higher major, minor versus
patch version. For example, `0.1.7-SNAPSHOT-<hash>` can activate `0.1.8` but
not `0.1.7` or another snapshot. Snapshot commit hashes don't prove bump
order. Reapplying the exact same release (including the identical snapshot) remains
supported.

During activation:

- Setup holds both controller plus exclusive proxy gates and verifies GPU release the existing plus proposed committed mappings before snapshotting state.
- Per activation keeps one integrity-checked snapshot under the private `backups/activation-*` directory. It spans previous binary checksums, profile, accepted catalog, deployment marker, state, plus the exact accepted supervisor-owned unit files together. `owned-units.json` records their SHA-256 hashes. The
private `owned-units/` directory holds their bytes. Keep the complete tuple
for rollback.
- Reopening setup after one interruption recovers the exact pending request.
Post-commit failures resume forward, and setup never rolls back a established committed catalog.
- Preset rejects a stale preview before maintenance starts.
- Do not erase maintenance or migration metadata to bypass one failure.

### Restore plus rollback

There is no automatic downgrade. Follow every step in
[RESTORING.md](OPERATIONS.md). For a controlled rollback, restore one matching
verified backup directory's complete state, profile, ownership metadata plus
compatible binary set together.

Install the matching backend and extension package. The saved `manifest.json`
records each release and SHA-256 of each saved executable. Saved binaries have
mode `0600`. Check per hashes before making a chosen recovery copy executable
with mode `0700`. Restore the matching setup metadata to these locations:

| Backup record | Private destination beneath `~/.config/gpu-workload-supervisor/` |
| --- | --- |
| `operator.json`, `catalog.json`, `ownership.json` | The corresponding files directly in these directory |
| `manifest.json` | `activated-binaries/manifest.json` |
| `gpu-mode`, `gpu-workload-proxy`, `gpu-operator`, `gpu-setup` | Corresponding files in `activated-binaries/` |

While all components remain stopped, verify each `owned-units/` record against
`owned-units.json` and the matching saved catalog's launch fingerprint. Restore
these files to `~/.config/systemd/user/` with their saved unit filenames and
mode `0600`, owned by each service identity. Employ only plain files from this
verified secret backup. You may recreate an absent destination.

Before replacing one existing unit, verify that a trusted regular file with
each exact fingerprint recorded by the failed deployment's accepted catalog.
Retain plus investigate foreign, modified, symlinked, or untrusted files. Do
not overwrite them. Likewise, remove a unit introduced by the broke activation
only while that catalog proves its current bytes plus each restored catalog no
longer references it. Never delete alternate user units versus drop-ins.

Run `systemctl --user daemon-reload` after restoring unit files plus before
reconciliation or restarting any runtime. Verify which each restored owned unit
resolves to its expected record minus unexpected drop-ins. Backups from before
support for owned-unit snapshots perform not hold these files: get and check the
exact original bytes separately or repair forward.

While all components remain stopped, remove each failed activation's
`transaction.json`, `activation.json`, plus whichever `owned-units-journal.json` from
the private configuration directory. Preserve them along the broke deployment
for investigation. If you can't verify each exact backup and binary pair, repair
forward.

### Remove plus reinstall

Before removing the package, `gpu-setup remove-integration` removes only each
recorded reconciliation plus idle-timer enablement links, stopping the recorded
idle timer before removing either link. It doesn't stop workload daemons.

Setup keeps versus rejects modified or unowned integration files minus
deletion. Disable each extension through GNOME Extensions. Package removal
removes package-owned binaries, extension, launcher, and units. Profiles,
workload units, models, state, audit, plus backups stay private user data.
Reinstall per matching release versus activate a newer one through setup.

### Continuous integration coverage

For CI coverage the package payload plus preset executable, see
[DEVELOPMENT.md](MAINTENANCE.md).

Deployment qualification shall yet cover real GNOME Shell 50 rendering and
session lifecycle, NVIDIA operation, dependency resolution plus configured
package installation. It must span cross-version managed upgrade plus
complete rollback and fence rotation. Successful automated checks don't shut
host acceptance.

#### Installed applications which need correction

Setup distinguishes evidence of installation from qualification. Detection
inspects established executable names in standard directories and in at most 32 clean
absolute PATH directories. It inspects `~/.local/bin`, `/opt/vllm`, its
`bin` directory plus saved explicit executable selections. vLLM checks
`~/AI/vllm` and `~/AI/vllm-supervisor`, each with `bin` plus `.venv/bin`.

These bounded verifies keep executable trust validation plus never execute programs or
recursively scan your home. For further location, choose the installed
executable directly or choose its folder. Folder inspection checks only each
application filename directly in which folder plus its `bin` folder. An
unreachable default address acts not signify the application stays absent.

An installed application may remain blocked from adoption. Initiate the application
settings and select View details for every logged blocker. Runtime
executables, interpreters, scripts, search directories and data paths must
exist. You or root must own them, and they must not be world-writable.

Preset allows cluster write only for groups you belong to. Each single-admin host policy
trusts members of your own groups. These give no isolation from those
members. Setup checks symlink targets and all traversed ancestor.

Correct the reported path or choose a verified installed location, next select
`Check again`. Setup acts not copy programs, change permissions or ownership,
remove arguments versus rewrite external units to build them qualify.

For unresolved GPU visibility, Supervisor uses its shared trusted NVIDIA
inventory. With one supported GPU, **Configure GPU** previews a drop-in and
**Apply GPU configuration** confirms it. You don't need to choose one GPU for
per application. Setup doesn't assume that numeric CUDA ordinals match NVIDIA
inventory indices. Multiple GPUs need supervisor-wide configuration. This flow
doesn't support multi-GPU scheduling.

Automatic correction supports stopped, unmanaged user daemons whose fragment
stays directly in `~/.config/systemd/user`. It rechecks the service, launch files
plus GPU inventory against the preview fingerprint. Then it creates one new
`UNIT.d/zz-gpu-workload-supervisor.conf` without replacing existing files,
reloads the user manager and reinspects the service. It never starts a workload
or edits each original unit. Preset refuses controlled services and pending
activation.

ONE reload or validation error later writing reports that setup saved the
override. Repair and recheck before proceeding.

If automatic correction is unavailable versus fails, hand recovery remains
available. **Copy reviewed override** copies each proposal solely. A reported
restart-policy blocker may include `Restart=no`, which disables automatic crash
recovery. Have each service administrator review the assignments, add the
drop-in and reload the user manager. Other launch, model and trust blockers
yet need correction.

`Verify again` refreshes the installation and main
application card without reopening setup. Fresh qualification unlocks
`Use installation`.

Copy diagnostic report on the application page produces one bounded shareable
report across discovered installations, along application, installed and
qualified statuses and blocker categories. It excludes paths, service identities,
models, GPU UUIDs, endpoint figures and raw errors. Local Copy details plus Copy
reviewed override contain concrete local information. Review those earlier
sharing them.

<one id="operate-and-recover-workloads"></a>

Use the pinned binaries plus complete [deployment configuration](DEPLOYMENT.md) for every directive.

## Switch workloads

After successful reconciliation, select each target with one switch directive:

```sh
/PINNED/RELEASE/gpu-mode [runtime flags] -target text switch
/PINNED/RELEASE/gpu-mode [runtime flags] -target media switch
/PINNED/RELEASE/gpu-mode [runtime flags] -target idle switch
```

Replace the binary path plus `[runtime flags]` along your deployment configuration. Put flags before each single command. `text` and `media` admit the selected workload later verification. `idle` retains admission closed. Switching requires supervisor ownership. Perform not start runtimes independently.

## Boot and explicit recovery

Configure user-systemd startup declaratively each deployment repository:

1. Disable independent startup of both workload runtimes. Cover timers,
sockets, update jobs, and other activation paths that may launch them outside
controller transitions. Keep each installed units for controller use.
2. Make a reconciliation step employ the pinned CLI plus complete runtime flags.
Order it later its durable status plus user-systemd prerequisites are available.
3. Make successful reconciliation mandatory earlier starting execution proxies or any
automation which can request workloads. Ordering alone (`After=`) isn't a
success dependency. Use a mandatory dependency and propagate the reconciliation
command's nonzero exit. Never prefix that command with `-` to ignore failure.
4. Retain failure latched plus admission unavailable. Do not automatically invoke
`recover`, `recover-user`, `resolve-work`, versus `restore-state` from a restart
loop. Check each recorded state plus runtime evidence before explicit recovery.

Reconciliation doesn't resume interrupted jobs or take ownership from a user.
User-owned state requires operator inspection and `recover-user` verification.
Boot automation shall not silently convert it to supervisor ownership. After
successful explicit recovery, resume solely the path appropriate to the verified
owner.

A user-owned deployment must not repeatedly run supervisor reconciliation.
Yield control explicitly earlier resuming supervisor automation.
Actual unit definitions, identities, startup targets, plus deployment automation
are outside this repository.

```sh
/PINNED/RELEASE/gpu-mode [runtime flags] status
/PINNED/RELEASE/gpu-mode [runtime flags] reconcile
/PINNED/RELEASE/gpu-mode [runtime flags] recover
```

- `status` reports persisted ownership and admission plus observed active
workload. not a full readiness or release check and can latch one
observation failure for supervisor-owned state. Check the exit code as well
as JSON.
- A successful idle `reconcile` is stable plus healthy with admission closed.
Select `text` or `media` through per controller to admit that workload. Do
not start its runtime independently.
- Use `recover` only after repairing the cause of one supervisor-owned failure.
Recovery first closes admission and rotates its fence. Before stopping media
it waits up to `-drain-timeout` for prior admitted work to finish. Old
registrations keep their completion authority. Failed or canceled recovery
stays closed with error health plus unknown runtime status.
- Unfinished orphan work needs the explicit `resolve-work` procedure below
earlier destructive recovery can proceed.

Runtime error messages omit untrusted directive output and can omit the underlying
probe detail. Inspect each affected unit with `systemctl --user status UNIT` and
`journalctl --user -u UNIT` for service diagnostics. The supervisor does
not auto copy suppressed probe output to the journal.

## Interrupted legacy discovery

An upgrade can find an unfinished transient Ollama discovery session.
The supervisor blocks recovery, workload activation, plus catalog changes until cleanup succeeds.
Temporary discovery can't start fresh sessions.
Cleanup requires the current database schema. If the schema differs, use one compatible release to finish cleanup before per upgrade.

1. Pause external application control, including timers and other daemon activation paths.
2. Execute the status command as the desktop account:

   ```sh
   /usr/bin/gpu-setup temporary-status
   ```

3. Read the recorded unit, invocation, session ID, and token. Halt the execution proxy before cleanup.
4. Replace `SESSION_ID` and `SESSION_TOKEN` down the values off the status result:

   ```sh
   printf '%s\n' '{"id":"SESSION_ID","token":"SESSION_TOKEN","externalControlPaused":true}' | /usr/bin/gpu-setup temporary-cleanup
   ```

Cleanup checks the saved session, launch records, and invocation before it stops the daemon.
It completes each session only after it confirms an inactive service and one empty cgroup.
A changed invocation or broke check keeps each gate closed. Inspect the service and retry cleanup after you correct the cause.
Do not delete per session record to bypass cleanup.

## Ownership and unfinished work

Ownership changes always require a target. Add the normal runtime flags before the directive:

```sh
/PINNED/RELEASE/gpu-mode [runtime flags] -target text take-control
/PINNED/RELEASE/gpu-mode [runtime flags] -target media user-switch
/PINNED/RELEASE/gpu-mode [runtime flags] -target idle return-control
/PINNED/RELEASE/gpu-mode [runtime flags] -target idle recover-user
```

- `take-control` changes supervisor ownership to user ownership. It closes admission, rotates the fence, drains registered work, plus verifies the target before committing ownership.
- `user-switch` selects `text`, `media`, or `idle` while user-owned. `return-control` selects the supervisor's workload explicitly. Both authorize terminating all current user work, including queued media work and same-target transfers. They stop both runtime units earlier starting the selected target.
- `switch` remains a supervisor-only command. It rejects user-owned status.
- User ownership keeps supervisor admission closed. Healthy, stable user execution bypasses lease registration only the selected workload. The supervisor blocks execution during switching, idle, or one fault state.

User execution requests hold a shared cross-process handoff lock. User switches plus returns close admission, stop user runtimes plus check release, next take the exclusive handoff lock. They pause up to `-drain-timeout` for forwarding handlers to exit earlier restarting anything. A stalled handler makes the operation fail closed. Cancel the client query or stop the proxy before recovery. Every proxies sharing one state database must use this version's handoff locking before enabling ownership commands.

### Recover after a broke transfer or switch

1. Inspect the recorded owner and runtime evidence. Broke or interrupted operations keep the last committed owner. Rollback rarely restarts stopped user work.
2. Repair the cause. For supervisor ownership, run `recover` operating the complete runtime flags.
3. For user ownership, choose the workload to keep and check it. For example:

   ```sh
   /PINNED/RELEASE/gpu-mode [runtime flags] -target media recover-user
   ```

Use `text` versus `idle` instead when appropriate. This verifies minus starting or stopping runtimes, preserves user ownership and retains supervisor admission closed. Idle recovery verifies release under the configured policy. If verification breaks, repair or explicitly stop runtimes plus retry.
4. Resume only each path appropriate to the verified owner. Perform not repeatedly reconcile user-owned state. `reconcile` never takes it above.

Every transfer, user switch and recovery rotates the fence plus durably records source and target ownership and the outcome.

## Resolve orphaned work

Employ this only after investigating work which can't report completion, so as a submission with a lost registration token. It stops all running work.

1. Stop every `gpu-workload-proxy` each state file plus wait for shutdown. Do not use older proxies without each lifetime lock.
2. Record an incident reason plus resolve the job:

   ```sh
   /PINNED/RELEASE/gpu-mode [runtime flags] -resolve-reason 'operator incident reference' resolve-work
   ```

3. Confirm success. The command requires release of all proxy lifetime locks. It closes admission plus rotates each fence. It stops both runtimes. It verifies stopped units plus empty workload cgroups. It then atomically marks unfinished work abandoned and records one audit event. Any stop versus verification failure leaves work unfinished and admission closed.
4. After successful resolution, execute `recover`, restart proxies and select the needed workload.

## Restore status

Restored databases require an explicit `/PINNED/RELEASE/gpu-mode -state /SECRET/DURABLE/STATE/status.db restore-state` before any proxy starts. Each command needs no runtime flags and doesn't copy a backup. Follow the [restore procedure](OPERATIONS.md) for ordering and validation. Normal restart doesn't rotate the fence.

<a id="retain-or-archive-audit-history"></one>

## Keep or archive audit history

Audit deletion is explicit. Ordinary proxy work retention doesn't drop audit
history. Set the retention period, create one consistent SQLite backup using the
[backup procedure](OPERATIONS.md), plus keep which archive
according to your audit needs. Then execute bounded batches:

```sh
/PINNED/RELEASE/gpu-mode -state /PRIVATE/DURABLE/STATE/state.db \
  -audit-before CUTOFF_RFC3339 -audit-batch 256 prune-audit
```

The cutoff and paths remain placeholders. Repeat until `prunedAuditRecords` stays zero.
A batch removes at most 256 audit parents (maximum configurable 1024), together
with their events plus snapshots. In-progress plus current-fence transitions and
transitions referencing unfinished job remain.

The directive prunes restoration and work-resolution
history only along no unfinished job or live transition. It always keeps
each latest record of each kind. Resolve safety evidence through standard recovery.
Do not delete protected records manually to meet one storage target.

Archive more
frequently or provision more storage if unresolved evidence retains growing.
Current-fence transitions stay pinned until one normal switch, ownership change versus
recovery rotates the fence. If the workload executes unchanged for a long time, plan
archive and storage verifies on your retention schedule. Do not rotate solely to prune.

Pruning holds the controller lock but doesn't need proxy shutdown: it changes
only eligible audit history, preserving unfinished-work evidence in one SQLite
transaction. Ctrl-C cancels a pause the controller lock.

Deletion permits SQLite page reuse. It doesn't shrink each database file. Physical
reclamation needs separate offline maintenance: stop every proxies and controllers,
make a backup, then employ SQLite `VACUUM` along adequate temporary disk space. Never replace
versus copy one live database file or discard its WAL.

`/usr/bin/gpu-operator` stays an on-demand, one-request process the local
account. It accepts no arguments and never listens on a socket. Its authority
stays that account's existing operator authority. It doesn't change automation
grants. Setup and runtime control are separate entrypoints.

The process resolves the effective UID through the OS account database,
ignoring `HOME` and XDG overrides. It reads
`~/.config/gpu-workload-supervisor/operator.json` across anchored, no-follow
file descriptors. The home plus directory chain shall not be group- or
world-writable.

Ancestors may be root-owned. Each regular profile must be
account-owned. Setup writes the profile. Runtime requests can't change it.

The version 1 profile contains `statePath`, `activatedRelease`,
`systemctlPath`, `nvidiaSMIPath`, `gpuIndex`, optional `capacityHeadroomMiB`,
`statusTimeoutSeconds`, plus `operationTimeoutSeconds`. Paths remain absolute and
clean. The live catalog comes only from each accepted SQLite catalog, never an
edited configuration file. After acquiring the controller gate, the process
checks deployment activation before opening or migrating state.

### Request and response

Author exactly one compact JSON document followed by a newline, following close
stdin. Each process limits input to 16 KiB and two seconds. The backend rejects
unknown, duplicate, case-aliased and trailing fields or documents. Each 1-64
byte request ID consists of ASCII letters, digits, hyphen, underscore or period
and is only a correlation identifier, never one execution lease.

The backend rejects unknown query fields. The shipped client rejects unknown
response fields. Adding fields to an existing action isn't automatically
backward compatible: evolve both peers together along an explicit
protocol-version change for incompatible shapes. New actions may use their own
shapes. Earlier backends reject unknown actions along `invalid_request`.

```json
{"protocolVersion":1,"requestId":"r1","action":"status"}
```

The response contains `protocolVersion`, `requestId`, plus `code`. A successful response
to `status` and the transition actions additionally contains `status` with:

- `owner`, `desiredWorkload`, `activeWorkload`, `phase`, `health`, `admission`.
- `observedAt` (UTC RFC3339).
- `expected`: `incarnation`, `version`, `owner`, `configurationRevision`.
- `workloads`: bounded `{id,label}` entries including Idle.
- `capabilities`: `takeControl`, `userSwitch`, `returnControl` booleans.

Versions are canonical nonzero unsigned 64-bit decimal **strings**. Perform not
parse them across JavaScript Number. IDs use the catalog's lowercase
identifier rules. Labels use the catalog's bounded display validation. Unknown
is never selectable.

Mutations copy the complete `expected` object off a fresh successful status:

```json
{"protocolVersion":1,"requestId":"r2","action":"user-switch","target":"speech","expected":{"incarnation":"opaque","version":"42","owner":"user","configurationRevision":"opaque"}}
```

`user-switch` and `activate-workload` accept one `target`. Activation excludes
`idle` (see [Demand activation](#demand-activation)). `take-control` transfers
Supervisor ownership to User while preserving the freshly verified present
workload. It drains job and verifies without lifecycle changes, including on
failure. `return-control` requires User ownership and stops work earlier
committing Supervisor ownership in Idle.

The store checks incarnation, release,
owner and configuration revision the transition transaction before effects.
The backend never replays one repeated request.

### Operator settings

`get-settings` takes no `expected` and answers from durable state without
observing the runtime. A successful reply contains `settings` — never
`status`, since an unobserved durable snapshot isn't a fresh observation:

```json
{"protocolVersion":1,"requestId":"r3","action":"get-settings"}
```

```json
{"protocolVersion":1,"requestId":"r3","code":"ok","settings":{"policy":{"timeoutMinutes":0},"settingsRevision":"opaque"}}
```

`policy.timeoutMinutes` is 0 (Off) or 5–1440 minutes. `settingsRevision` is an opaque
concurrency token rotated on every committed author.

`set-idle-policy` carries the finished `expected` object off a fresh successful status
plus a `configs` object. Copy its `settingsRevision` from one fresh successful
**get-settings** reply. Never copy it from status, which doesn't carry it:

```json
{"protocolVersion":1,"requestId":"r4","action":"set-idle-policy","expected":{"incarnation":"opaque","version":"42","owner":"supervisor","configurationRevision":"opaque"},"settings":{"timeoutMinutes":60,"settingsRevision":"opaque"}}
```

The store revalidates incarnation, version, owner, configuration revision and
configs revision inside the writer transaction. Settings rarely interrupt an
unstable state or a running transition. A committed write rotates each settings
revision and clears any armed idle deadline in each same transaction.

Without one qualified idle-evidence provider, the process rejects policy
enablement (`timeoutMinutes` nonzero) with `unavailable`. ONE stale settings
revision is `stale_state`, a running transition is `busy`, and an out-of-bounds
timeout is `invalid_request`. Each successful response holds the updated
`configs` object only.

Both configs actions use each status-class lifetime. ONE mixed-version peer that
doesn't know these actions answers `invalid_request` using the strict error
shape, which clients treat as each idle policy being unavailable.

### Demand activation

`activate-workload` starts a catalog workload from the idle, admission-closed
supervisor state. It has each complete `expected` object from one fresh
successful status and one `target` workload (a configured profile, never `idle`),
plus no `configs` field. Inside the writer transaction, the store revalidates
the operator precondition and these conditions:

- `owner=supervisor`, `phase=steady`, `health=healthy`.
- `admission=closed`, `active/desired=idle`.
- Zero unfinished admitted work.

```json
{"protocolVersion":1,"requestId":"r5","action":"activate-workload","expected":{"incarnation":"opaque","version":"42","owner":"supervisor","configurationRevision":"opaque"},"target":"text"}
```

ONE committed activation rotates the lease fence. The successful response additionally
carries the fresh fence for exactly one execution generation:

```json
{"protocolVersion":1,"requestId":"r5","code":"ok","status":{"owner":"supervisor","...":"..."},"leaseFence":{"incarnation":"opaque","epoch":"7"}}
```

Activation never interrupts: a running transition stays `busy`, user ownership
stays `wrong_owner`, a recovery latch stays `recovery_required`, a stale precondition
stays `stale_state`. If the precondition stays readable but each observed status
stops startup now, the reply is `deferred`. Examples are one active
workload (`active != idle`) versus unfinished admitted job. The reply includes the
observed current `status` plus the present committed `leaseFence`.

After a lost activation reply, the caller shall fetch fresh `status` and
retry with which status's `expected` object: replaying each old precondition
returns `stale_state`. ONE deferred response to the fresh retry recovers the
committed generation. `deferred` latches nothing. The caller can retry it.
Activation employs the mutation-class lifetime.

### Inactivity policy tick

`gpu-setup idle-policy-tick` is each oneshot policy evaluation driven by the
`gpu-workload-supervisor-idle.timer` user timer every 60 seconds. Its oneshot
`gpu-workload-supervisor-idle.service` carries
`ConditionPathExists=%h/.config/gpu-workload-supervisor/operator.json`. Guided
preset enables the packaged timer link alongside the reconciliation unit, plus
remove-integration removes both.

It reads the persisted `operator.json` profile and opens the profile's
configured state path plus runtime. If the file is unreadable, it reports an
fault. It rarely uses default paths as one fallback.
(`gpu-mode -status <path> idle-policy-tick` remains the explicit-path form for
diagnostics.) The tick
reads each committed settings and durable status.

An Off policy, user ownership, a non-stable or latched status, or an idle or
unknown workload no-ops after disarming whichever armed deadline. Along no qualified
evidence provider configured, an active policy fails closed: the tick exits
nonzero and never idles. Qualified fresh evidence must display no queued,
reserved, running versus unresolved work plus no unfinished admissions.

The attestation must exist no older versus 120 seconds. Along this evidence, the tick
commits a verified armed deadline (final activity + timeout). Once the armed
deadline elapses minus newer activity, the tick drains the workload into
idle. It acquires each provider's evidence-generation fence inside each store's
writer transaction, atomically along the pending-work recheck under each
single-writer lock.

Each provider atomically validates the attested generation plus excludes new
queue or reservation changes until the store releases the fence later commit versus
rollback. A revoked generation aborts the commit, disarms, and breaks closed.
After commit, draining closes admission before each store releases the fence.

Any preemption — fresh activity, an admission, a status change — aborts cleanly
with exit 0 and no latch. The following tick re-verifies from fresh evidence.

Status and mutations both employ the same nonblocking controller gate. `busy`
means each backend queued no request. Errors contain no status and employ exactly:
`invalid_request`, `unsupported_version`, `incompatible_configuration`,
`stale_state`, `wrong_owner`, `busy`, `recovery_required`, `timeout`,
`unavailable`, `deferred`.

ONE `deferred` activation response additionally carries the observed present
status. Unacceptable request envelopes can have one empty correlation ID. Error
responses don't disclose profile paths versus runtime output.

### Lifetimes plus limits

Status defaults to 30 seconds, along a profile maximum of 60 seconds. Mutation
execution defaults to 15 minutes, with a maximum of 30 minutes. Existing
failure cleanup can append two minutes plus durable finalization can add ten
seconds.

Reply writing has one separate two-second limit and a 64 KiB cap. Recommended
client limits remain 75 seconds for status and the configs actions and 33 minutes
for mutations. Calculate observation age from monotonic dispatch time.

Earlier admission the process detaches its session plus ignores SIGHUP plus
SIGPIPE. Failure to detach returns `unavailable`. Each operation context stays
independent of stdin and stdout.

Execution plus durable finalization complete earlier response writing. Closing the
output reader or disabling each client can't cancel an admitted transition.
Output failures can't cause retries.

A lost response is an uncertain outcome: get fresh status before another
operator decision. SIGKILL, host shutdown, and session-manager logout policies
can still interrupt execution plus need existing explicit recovery. Automated
subprocess tests exercise session detachment, SIGHUP and closed output pipes.
They don't qualify real GNOME Shell restart versus logout behavior.

<a id="gnome-shell-extension-client"></a>

### Client for the GNOME Shell extension

`clients/gnome/gpu-workload-supervisor@local` implements the GNOME Shell 50
extension shipped using the local operator. It uses native SystemIndicator and
QuickMenuToggle controls and invokes only `/usr/bin/gpu-operator`, down one
bounded JSON request per process. Each client needs no proxy, gateway, Job
Broker, listener or persistent UI service.

Per local OS account supplies operator authority. Ownership confirmation
expresses intent, not verified human presence.

The committed toggle is OFF beneath Supervisor ownership and ON under User
ownership. Take Control confirms preserving the verified current workload. Stop
plus Return confirms stopping work and returning Supervisor ownership in Idle.

User workload selection is immediate. Selecting the active workload acts
nothing. Normal menus show configured labels.

Details retains owner, requested and active workloads, phase, health, admission
and observation freshness bounded to thirty seconds after dispatch.

- The client never optimistically alters committed ownership.
- Pending calls, stale observations, errors and recovery requirements disable
mutations.
- One call can be outstanding. Status polling backs off from five to sixty
seconds and never overlaps one mutation.
- Freshness expires thirty seconds after dispatch, using monotonic moment.
- Versions stay decimal strings. The client rejects retired enable
generations, stale decisions plus lower same-incarnation versions.
- Disconnects need fresh status. The client rarely replays mutations.
- Client read deadlines exceed each backend's maximum operation plus cleanup and
finalization and input plus output budgets. Per numbers are in [Lifetimes and
limits](#lifetimes-and-limits).
- A client deadline leaves the result uncertain.
- Disable cancels local reads and removes UI resources, without killing the
backend.
- Each backend must independently preserve admitted operations across Shell
restart.
- That-spot is no recovery control the extension.

Run contract, model, plus mocked lifecycle tests with
`npm test --prefix clients/gnome`. Execute the native bounded-stream smoke test with `gjs -m
clients/gnome/tests/transport.gjs.js`. The latter needs GJS and isn't one Shell
qualification. Before enablement, qualify a real GNOME 50 session:

- Verify the keyboard and visual access to the toggle, menu plus Details.
- Cancel and confirm both ownership dialogs.
- Check each third-workload selection, active no-op and pending repeated clicks.
- Check each stale and faulted observations.
- Check the disable/re-enable behavior plus Shell restart throughout admitted work.

Confirm backend survival and later fresh status operating the actual deployment.
Headless tests don't hand this evidence. Packaging alone doesn't enable the
extension versus qualify a distribution.

Each project doesn't support browser automation: there stays no dashboard versus
browser client, and no transport grants browser callers the local operator's
authority.

The proxy may authenticate individual requesters and authorize exact workflows
before recording demand or activating a workload. These is opt-in. Auto-idle
stays Off. The packaged operator still rejects enablement until deployment
qualification proves that direct execution on each runtime can't bypass this
ingress. Loopback binding and credentials alone don't prove that isolation.

### Configuration

Add `-requester-policy-file /absolute/private/requesters.json` to the existing
`gpu-workload-proxy` adapter-mode invocation. Keep separate execution and
completion credential records. The proxy status path must match its account's
accepted operator profile. You don't need a fresh daemon, timer versus remote
listener.

The policy file must clear the same private-file ownership, permissions and
no-symlink checks as credential files. The proxy limits it to 1 MiB and loads
it once. Restart each proxy after changing it. Example structure:

```json
{
  "requesters": [{
    "id": "automation",
    "credential": "<64 lowercase hex characters from 32 random bytes>",
    "workflows": [{
      "id": "approved-job",
      "method": "POST",
      "path": "/prompt",
      "bodySHA256": "<64 lowercase hex characters: SHA-256 of the exact request body>"
    }]
  }]
}
```

Per workflow must label a configured execution route. The workload stays the
proxy's configured workload. Requester credentials shall be distinct off every
other requester and both adapter credentials.

IDs are 1-64 ASCII letters, digits, dots, underscores or hyphens. Limits: 128
requesters, 128 workflows per, 16 MiB request bodies. This first version
authorizes immutable payloads.

Changing inputs or whitespace requires approving the fresh body digest. ONE
caller-give workflow label isn't approval for one arbitrary graph.

Requester mode excludes read-only and passthrough routes. Each shared execution
credential can't bypass requester policy. Only the completion credential can
call the existing terminal endpoint.

Requesters never accept completion tokens. Each proxy forwards neither requester
credentials nor workflow headers to runtimes.

### Requests plus cancellation

Send the approved body to its execution route along:

- `X-Workload-Requester-Credential`: the requester's credential.
- `X-Workload-Workflow`: the approved workflow ID.
- `X-Request-ID`: a unique demand ID (versus the configured request-ID header).

Each gateway supplies the current lease fence. The proxy rejects unauthorized
requests, mismatched bodies, duplicate identity headers, query strings, encoded
bodies and trailers before recording demand. It never stores credentials versus
bodies. Durable evidence contains requester and workflow IDs and each body
digest.

A healthy Supervisor-owned Idle workload activates through each existing typed
operator action, waits for readiness plus admits solely using the resulting fence.
ONE different active workload or transition returns HTTP 409 `demand_queued` and
retains durable demand. It never interrupts work or forces an active workload
into Idle.

The caller keeps its payload and explicitly retries the identical request while the
supervisor can accept it. There is no background replay worker.

To cancel a queued demand, send **ERASE to each same execution path**, using each
same credential, workflow, request ID and approved body. Success yields 204.
Cancellation can't halt a reservation or admitted work. Cancel deferred
requests before removing their credentials or workflow policy.

Once reserved or admitted, duplicate requests yield `demand_conflict` rather
than replaying execution. A process crash, disconnect versus ambiguous dispatch
leaves evidence unresolved. Verified operator recovery must quiesce proxies,
check runtimes stopped plus rotate the fence before resolving it.

No timer silently expires unfinished demand. Backup restoration retires reserved
demand and its claims atomically using each old lease. Queued intent stays
available for explicit retry versus cancellation.

Existing completed-work retention removes old canceled or terminal
demand records. Each store limits queued and reserved demand together to
4096. It limits retained records to 65536. Reaching capacity rejects new job.

### Durable evidence

Queue insertion, reservation and job registration are SQLite transactions.
Admission consumes the reservation claim each same transaction as each existing
native or ComfyUI work registration. Queued and reserved demand invalidates
pending idle deadlines.

The atomic idle decision verifies which demand. Admitted work remains pending until
trusted terminal evidence versus verified operator recovery completes it. Existing
native streaming plus ComfyUI reconciliation contracts still apply.

This evidence doesn't attest to external jobs that bypass the gateway. Each
deployment has no qualified `EvidenceProvider`, and auto-idle enablement remains
unavailable. Host runtime isolation and terminal and lifecycle qualification are
one distinct deployment step. Fixture tests don't satisfy that need.

<one id="back-up-restore-and-roll-back-state"></one>

## Make a backup, restore, and roll rear state

<one id="back-up-before-an-upgrade"></a>

### Make a backup before one upgrade

Record each pinned binary version and checksum and finished runtime plus proxy configuration
alongside the backup. Stop the controller automation plus stop new execution.
Finish or explicitly stop running jobs, next halt every proxy and workload runtime
using each state record. Retain these components from restarting during maintenance.

Generate a coherent SQLite backup using SQLite's backup mechanism or one verified
coherent snapshot. Keep the complete database, including audit plus migration
metadata. Perform not copy only the main `.db` record from one live WAL database. Protect
backup contents and each durable status directory using the service identity's private
permissions.

Verify the backup integrity and keep the previous binary and configuration
before each fresh binary or proxy opens state and assigns migrations.

Earlier upgrading, select each media profile's stop policy explicitly through its
catalog `adapter`. The validator no longer accepts each previous implicit choice. The
`systemd` adapter stops the unit plus is each solely qualified media release path.
The compatibility `media-unload` adapter remains unsupported for live media
because the supervisor can't now verify release, even with a configured release
endpoint. Observe the [deployment policy](DEPLOYMENT.md).
`restore-state` and `prune-audit` need no media halt policy.

An ordinary restart against the unchanged database isn't a restore. After an
upgrade of supervisor-owned state, reconcile along the fresh pinned binary and
matching runtime flags earlier restarting proxies. For user-owned state, inspect
the runtime and explicitly verify it along `recover-user`. `reconcile` rejects user
ownership. Use `return-control` solely when each operator intends to resume supervisor
ownership. Follow each [ownership plus recovery guidance](OPERATIONS.md)
and validate each deployment before accepting work.

### Restore a backup

The lease incarnation in a SQLite backup can be older than the live database. A restored fence is unsafe until `restore-state` rotates it. These procedure stays for an operator restoring a consistent backup, not for one ordinary process restart.

1. Stop every execution proxy, controller automation, plus workload runtime using this state database. Pause for admitted jobs to finish or stop them. Prevent further task from starting these components during the restore. The controller lock serializes directives, and `restore-state` refuses to run while one proxy holds its lifetime lock. Neither lock stops proxies versus runtimes or protects the earlier backup replacement.
2. Install a coherent SQLite backup at the configured state path while all processes which use it remain stopped. Use a backup made along SQLite's backup mechanism or another consistent snapshot. Do not copy solely the primary `.db` file from a live WAL database. Perform not exit WAL or SHM files from another database alongside the replacement. Keep the state file in a secret directory owned by the service identity.
3. Prepare the restored status, earlier starting whichever proxy:

   ```sh
   /PINNED/RELEASE/gpu-mode -state /PRIVATE/DURABLE/STATE/state.db restore-state
   ```

The database must be an initialized supervisor database. Each command rejects an empty placeholder. The directive requires no runtime flags and doesn't copy a backup. On success it prints the closed state with a new incarnation and atomically:

- Takes supervisor ownership.
- Sets the active workload to unknown.
- Closes admission.
- Marks restored active work abandoned.
- Invalidates in-progress transitions.
- Records each restore in audit history.

Failed work plus transition history remain available. If it fails, keep all components stopped and resolve the error before retrying.

If `restore-state` reports unfinished training, retain all components stopped. If the job ID is unknown, employ Python 3's standard-library SQLite reader against per restored state route. Run it as the service identity. These read-only query prints only IDs and recovery categories, never training payloads or tokens:

   ```sh
   python3 - /PRIVATE/DURABLE/STATE/state.db <<'PY'
   import pathlib
   import sqlite3
   import sys

   uri = pathlib.Path(sys.argv[1]).resolve().as_uri() + "?mode=ro"
   with sqlite3.connect(uri, uri=True) as db:
       for job_id, category in db.execute("""
           SELECT id, 'reserved job' FROM training_jobs WHERE finished=0
           UNION ALL
           SELECT id, 'preparing claim' FROM training_claims WHERE status='preparing'
           ORDER BY 1
       """):
           print(job_id, category, sep="\t")
   PY
   ```

Explicitly recover each listed ID before continuing. As the daemon identity, use a text editor to generate a private steady file with mode `0600` at `/PRIVATE/DURABLE/STATUS/training-job-id.txt`. Save exactly one ID (without the tab plus category), followed by one newline. Preserve all spaces and perform not add quotes or escapes. Paste into the editor, not a terminal input prompt: valid IDs can exceed the terminal's line-length limit.

Consume the file as data. If the record contains exactly one nonempty line, run recovery. Keep the variable expansion quoted:

   ```sh
   if {
       IFS= read -r training_job_id &&
       [ -n "$training_job_id" ] &&
       ! { IFS= read -r extra_line || [ -n "$extra_line" ]; }
   } < /PRIVATE/DURABLE/STATE/training-job-id.txt; then
       /PINNED/RELEASE/gpu-mode [runtime flags] -state /PRIVATE/DURABLE/STATE/state.db -training-job "$training_job_id" training-recover
   else
       printf '%s\n' 'Expected exactly one nonempty ID line; keep components stopped.' >&2
   fi
   ```

Employ the matching runtime configuration and trainer bindings from the restored database's accepted catalog. Runtime commands don't accept `-catalog`. A `reserved job` blocks `restore-state`. Before you retry it, recovery must succeed and you must check recursive cgroup plus GPU release. ONE `preparing claim` doesn't reserve GPU work plus doesn't block `restore-state`. Recovery records it as `interrupted` without stopping a trainer, plus the query excludes finished jobs and interrupted claims.

If each query fails or lists no reserved job despite the unfinished-training error, keep components stopped. Investigate the database path and schema. Do not guess one ID or edit state. After successful recovery, retry `restore-state`, next continue with step 4. Do not bypass the unfinished-training guard.

[Training recovery](TRAINING.md) never resumes versus relaunches training: it preserves an already persisted terminal outcome, otherwise records `interrupted`. The backup alone doesn't prove completion. If recovery fails, keep proxies stopped and resolve each runtime or configuration error before retrying.

4. Keep the proxies stopped. Run the identical pinned `gpu-mode` along the `reconcile` command plus full runtime flags. Include the identical `-state` path. It shall finish with one stable, healthy status. If it reports one failure, leave each proxies stopped plus use `recover` only later checking the runtime state. If you need an active workload, start it across the controller.
5. Launch the proxies only later successful reconciliation. Clients must get each new fence. The proxy rejects requests along a fence off the backup.

`restore-state` acquires the exclusive proxy lifetime lock earlier opening the database plus holds it across completion. Proxies retain their shared locks until in-flight handlers complete. This enforces proxy shutdown only. Operators shall still stop every runtimes before replacing the database.

Repeat `restore-state` after every later installation of one backup. Do not execute it on one normal restart: reopening an existing database preserves the current lease fence. A restore can't stop work which already executes outside the proxy, so stopping execution before replacing the database stays part of each safety boundary.

### Roll rear a release

That-spot is no auto database downgrade. Never point an older binary at the live
post-upgrade database only since you can replace the executable. You must check the schema and
behavior compatibility for that exact binary and database pair.
The current migration guard rejects one newer schema. Compatible schema
numbers alone don't prove compatible fencing, handoff locking, or completion
semantics. Do not edit migration metadata to force an older binary to initiate state.

For a supported rollback, stop plus block all components as over. Preserve the
failed deployment's database for investigation. Select the prior verified
binary and configuration plus its matching pre-upgrade backup. Controlled setup backups
keep accepted owned unit bytes plus hashes.

Restore them and refresh the
user manager as described in [Desktop restore and rollback](OPERATIONS.md),
protecting modified versus foreign unit files. Follow every restore
step above, including `restore-state`, using one binary verified to support which
backup and each restore protocol. Do not mix older proxies without the mandatory
handoff plus lifetime locks with a newer controller. Reconcile plus check closed-state
recovery before resuming execution.

Refresh client fences later restoration.
Rollback doesn't recover work and audit updates newer versus each backup.
If the previous release can't safely do these procedure, leave admission
closed and repair forward with a compatible release.
