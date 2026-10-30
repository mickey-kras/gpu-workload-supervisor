# Deploy and use

## Demand activation and inactivity idle plan

Implementation and remaining qualification for [issue
#144](https://github.com/mickey-kras/gpu-workload-supervisor/issues/144). Durable
policy, typed settings, demand activation, and a 60-second user timer exist.
Policy defaults to Off.

Packaged entrypoints have no qualified evidence provider: enabling returns
`unavailable`, and policy evaluation fails closed. Keep Off for initial desktop
deployment.

### Placement

- One policy loop in the supervisor backend, next to the transition controller. Never in GNOME Shell or any client.
- Reads durable evidence only. Drives existing typed controller operations. No second lifecycle engine, no new authority, no runtime-specific logic.
- Desktop integration must display committed policy and only a verified pending-idle deadline from backend state. UI disabled or session locked: no behavior change.

### Required before enablement

1. Deploy and qualify an orchestration `EvidenceProvider`. Contracts alone are not adapters.
2. Check the evidence for queued, reserved, running and unresolved work and stop direct execution bypass at the deployment boundary.
3. Qualify native terminal completion and model lifecycle on the deployment host. Fixture coverage is not host acceptance.
4. When backend capability and fresh evidence support it, integrate the desktop presentation of settings and deadlines.

[requester ingress](OPERATIONS.md#optional-requester-ingress) describes the optional authenticated requester and workflow
ingress and the durable queued and reserved demand. Production
isolation qualification remains outstanding. Auto-idle stays Off.

### Activation ingress

The typed local action `activate-workload` activates workloads on demand. See
[the operator protocol](OPERATIONS.md#local-operator-protocol). It requires a fresh state precondition and returns the new
lease fence only after the controller verifies readiness and admission. There is no
remote control facade.

- Queue and job state stays with orchestration until activation completes.
- Adapters get the fresh fence. Stale-fence registrations remain rejected.
- User ownership, busy transitions and recovery latches reject activation.

### Activation

- An authorized request under Supervisor ownership in Idle starts a switch. The controller checks readiness and opens admission.
- Mutual exclusion preserved. Waits for other work, never interrupts. Queue and job state stays with orchestration.
- Activation requested during active work: the request waits with orchestration. The supervisor returns a typed busy or deferred response. No interrupt, no preemption.
- The controller rejects activation during User ownership, a transition in progress, or a recovery latch. Nothing activates implicitly.

### Idle transition

Both required, committed as one fenced decision (state version and lease fence):

1. Inactivity period elapsed since last recorded workload activity.
2. No queued, reserved, running or unresolved work, including admitted registrations without terminal evidence.

Admission between check and switch aborts the transition. No shutdown race.

### Evidence input contract

The policy loop uses one attestation from the orchestration adapter. No other
evidence source.

1. Content: queued, reserved, running, and unresolved work sets, each as a set of job identities. Unresolved maps to store primitives: `PendingWork` (registrations retaining completion authority). `ResolveUnfinishedWork` stays reserved for verified operator recovery, never for policy cleanup.
2. Pull `Attest` on each evaluation. `AcquireFence` must validate its generation and exclude queue or reservation changes through the idle transition transaction. Invalidation cannot replace fresh evidence.
3. Freshness: the attestation has an adapter timestamp. The policy treats attestations older than a bounded staleness limit as missing. Implementation: 120 seconds (`MaxAttestationAge` in `internal/supervisor/policy.go`). The user timer ticks every 60 seconds (`packaging/gpu-workload-supervisor-idle.timer`).
4. Adapter outage or unreachable adapter -> fail closed: evidence counts as missing, no idle transition.
5. Uncertain, partial, or missing attestation -> no idle transition.

<a id="ownership-transfer-while-pending-idle-is-armed"></a>

### Ownership transfer with an armed idle deadline

- Any ownership transfer (take-control by User, or return-control completing) cancels and disarms a pending idle deadline.
- A disarmed deadline recomputes only from fresh post-transfer activity evidence.
- Never idle User-mode work. The policy loop exits while Owner is User.

### Activity definition

- Counts: admission registrations, verified terminal completion.
- Never counts: UI polling, health checks, keyboard or session input, GPU use, VRAM level.
- Loaded-but-unused = inactivity. A disconnect without terminal evidence means unresolved work.

### Settings

- One setting: timeout minutes + Off. Proposed 60. Enabled default only after host measurement below.
- Persisted through `get-settings`/`set-idle-policy`, with state and settings revision checks. On restart, the policy recomputes the pending deadline from durable last-activity. It never uses a process-local timer.
- Uncertain or missing evidence -> no idle transition.

<a id="status-surface-work-item"></a>

### Status interface

- `control.State` carries policy and deadline read-model fields. Committed operator settings are available through `get-settings`. Ordinary operator `status` is not a settings snapshot.
- Desktop policy and deadline presentation stays a follow-up. Expose a pending deadline only from verified backend evidence.
- No savings estimate, no live countdown. Countdown is UI-side rendering of the deadline only.

### Boundaries

- Never reclaim User ownership. Never idle User-mode work.
- No automatic recovery from a latched error. A latch blocks all policy transitions.
- No firmware, clock, power-limit or system-suspend changes.
- Host identity, runtime routes, measurements, installation: private deployment configuration.

### Acceptance

Test cases:

- Request at exact timeout, cold activation, activation during active work, mixed workloads, streaming and async completion.
- Long jobs crossing timeout, disconnects, ambiguous completion, adapter outage and stale attestation.
- Restart with pending deadline, ownership transfer with armed deadline, rejected requests and recovery latches.

Measure the power for loaded-but-unused and idle states and activation latency on the deployment host. Record in the private qualification record before any savings claim or enabled default.

## Local GNOME desktop installation

[Documentation](README.md). [Operator protocol](OPERATIONS.md#local-operator-protocol). [Restore
procedure](OPERATIONS.md#make-a-backup-restore-and-roll-back-state).

The Debian package contains the compatible CLI, proxy, one-shot operator
backend, GNOME extension, and native Gtk4/Libadwaita setup application. GNOME
Shell **50** is the initial target. The package requires GJS, Gtk4, Libadwaita,
and systemd.

CI builds Linux amd64 and arm64 artifacts. These build targets do not qualify a
particular Debian-family distribution or desktop and GPU combination. Real
Shell 50 session, logout and login, NVIDIA, upgrade, and package-manager
acceptance must pass on the deployment host before enabling production
controls.

### Install and configure

Install the verified release `.deb` with the distribution package manager, then
open **GPU Workload Setup** as the desktop account. Package installation
installs files only: no maintainer scripts open user databases, enumerate
sessions, stop workloads, or enable services for arbitrary users.

Choose applications, choose models only when relevant, then finish. Setup
presents ComfyUI and vLLM once each, with the model choice under its
application, and selects one unambiguous recognized installation.

Other installation controls remain future work. ComfyUI workflows select their
own models. Applications and models must already exist.

Review the ready-to-finish summary after active jobs finish, then select
**Finish setup** to confirm activation. ComfyUI closes when you switch to
another workload. Labeled gears expose service, endpoint, launch and resource
settings for inspection or supported overrides. Automatic and manual
configuration use the same validation and activation safeguards.

Setup refuses stale configuration, edited integration files, live workloads,
unfinished transitions and unsafe ownership or state relocation. Discovery and
preview never start applications or load models. Setup enables reconciliation
for future logins without `--now`. It is a oneshot, not a UI daemon.

A new system-wide extension installation can need logout and login before GNOME
discovers it. Enable **GPU Workload Supervisor** in GNOME Extensions afterward.
Setup rejects an unsupported Shell version or session.

Shell 50 discovery and visuals need real-session qualification. Headless tests
do not prove it.

The effective account's OS home determines the private profile location:
`~/.config/gpu-workload-supervisor/operator.json`. Environment HOME and XDG
overrides do not choose runtime configuration. The accepted workload catalog
and its opaque revision live in SQLite.

Setup owns `catalog.json` and uses it only for setup and backup. Ordinary
controls never adopt edits to that mirror. Setup exposes `discover`, `probe`,
`correct-gpu`, `prepare`, `fingerprint`, `render-owned`, `drafts`,
`save-drafts`, `verify-bindings`, `validate`, `apply`, `reconcile`,
`idle-policy-tick` and `remove-integration`.

Request and response commands use a strict versioned JSON protocol over stdin
and stdout. The desktop application supplies these requests and renders the
concrete preview.

### Manage workloads in setup

Open **Manage workloads** from GPU Control in GNOME's top-right Quick Settings
menu, or open the packaged setup application from the application list.

1. Select application cards. Select **Continue**.
2. Choose one model for each application. Select **Continue**. ComfyUI skips
   model selection. Discovery refresh reads metadata without startup.
3. Review the summary, finish active jobs and switch to Idle before **Finish
   setup**. Setup verifies current evidence and uses the existing activation
   safeguards.

**Save selections for later** keeps choices without making them selectable
workloads. Missing or unsupported installations offer a location or address
fallback and explain what evidence is missing. Custom wrappers and ambiguous
configuration need a supported launch or validated Advanced configuration.
Setup does not guess ownership from a healthy endpoint.

An application's gear opens its settings at the beginning of the view. **Back**
returns to the originating screen and gear while keeping selections. Candidate
choices show the service or address and its status. The chosen
candidate's full identity is also available in wrapped selectable text.
`Use installation` checks and reviews the selection. Only **Finish setup** activates
it.

An unreachable default address does not prove an installation. Installed but
stopped services, missing locations, missing model choices and inspection
failures have separate guidance. **View details** opens a separate
Configuration details page with the reported configuration problems, **Copy
details**, and `Check again`.

Independent launch checks report several blockers in one inspection. Checks
that depend on invalid or unreadable evidence need another inspection after
repair. Setup cannot activate a blocked installation. Discovery does not start
applications or change service files.

Supported external launches can use `%h` at the beginning of path tokens in
`ExecStart`, `Environment` and `WorkingDirectory`. It resolves from the desktop
account's unique effective-UID record in trusted root-owned local `/etc/passwd`
and must agree with systemd's loaded configuration. Setup does not support
missing local records, including NSS-only accounts. Ambient `HOME` is never a
fallback. Other expansion syntax stays unsupported, and expanded paths still
undergo the same ownership, permission and symbolic-link checks as literal
paths.

For installed vLLM without an existing supported service, open **Change
installation…** and choose **Choose installed executable…**. Return to
**Advanced settings** and choose **Review changes** to choose an existing model
directory. The next **Continue** validates the executable and model and
previews a Supervisor-managed launch.

**Finish setup** explicitly permits Supervisor to start and stop it. The
preview does not. **Advanced settings** holds common fields and opens
separate **Resource checks** and **Launch details** pages. **Launch details**
separates managed launch options from adopting an existing external service.
Canceling a file chooser keeps the previous selection. ComfyUI continues to use
its existing service configuration.

Setup adopts existing recognized service files and supported drop-ins without
changes to their launch options. Optional gear settings keep the
managed-launch and explicit-binding workflows. Setup never installs, updates or
downloads applications, environments or models.

vLLM external services keep the model pinned by their launch: choosing another
model does not rewrite their service. Each model needs its own service.

New or edited adopted services receive read-only launch and cgroup preflight,
followed by complete activation and GPU release checks. The backend
`render-owned` command returns a preview only. The backend `apply` command
creates the managed launch files.

See [configured workloads](INTEGRATION.md#configured-workloads). The controller checks model identity again
before GPU admission.

You can rename or edit configured workloads. **Edit application** reopens the
same setup flow and retains the workload identity and unrelated settings.
**Remove from supervisor** changes only supervisor configuration. You cannot
remove active or referenced workloads. Finish their work and switch to Idle
first. Setup preserves applications, models, workflows and external
configuration.

`Set up later` closes the window without applying configuration. **Save
selections for later** first if you want to keep new choices. A stale-draft
error requires reopening Manage workloads before retrying. Interrupted
activation resumes its recorded configuration, with editing disabled until it
completes.

Native GTK keyboard and accessibility checks and real GPU qualification remain
part of GNOME and package host acceptance. See [issue #146](https://github.com/mickey-kras/gpu-workload-supervisor/issues/146)
and [issue #148](https://github.com/mickey-kras/gpu-workload-supervisor/issues/148).
Automated widget and protocol tests do not qualify the host installation.

### Application discovery

`gpu-setup discover` returns saved setup state plus `applications` candidates
for ComfyUI and vLLM. It probes two default loopback ports and inspects
recognizable user-service launch metadata. Default endpoint observations remain
separate from service ownership.

A recognized service derives its endpoint and launch identity from loaded
metadata and its unchanged supported launch file. Existing catalog profiles
stay unchanged. Discovery reports errors separately from an empty inventory.

For a non-default endpoint or explicit reference:

```sh
printf '%s' '{"app":"vllm","endpoint":"http://127.0.0.1:8000"}' | gpu-setup probe
printf '%s' '{"app":"vllm","reference":"/models/model","referenceKind":"model-directory"}' | gpu-setup probe
```

`app` accepts `comfyui` and `vllm`. Select exactly one `endpoint` or
`reference`. `referenceKind` accepts `application`, `application-directory`,
`configuration` and `model-directory`.

Endpoint values are loopback HTTP(S) origins without credentials, path, query
or fragment. References must be absolute clean paths. vLLM `application`
selections allow executable aliases only when the target and every link hop
pass executable trust checks.

Configuration, model, directory and ComfyUI references accept only regular
files or directories and reject symbolic links. The probe does not execute or
inspect file contents. For vLLM, `application-directory` checks only known
application filenames in the selected directory and its `bin` directory. It
does not recursively discover files.

| Application | Read-only observations |
| --- | --- |
| ComfyUI | `/system_stats` version. Workflows select models, no model picker |
| vLLM | `/version` and `/v1/models`. Same-root aliases grouped. Model references in simple launches |

Missing optional APIs report `unsupported` or unknown loaded state. Unreachable
instances never become successful empty inventories. A verified stopped unit is
`not-running`. A failed endpoint connection is `unreachable`.

Saved references survive either condition. Served identities do not prove a
disk inventory or distinct physical models. File selection does not show
compatibility.

All candidates keep `lifecycleControl: "unverified"`. `recognized` and
`configurationStatus` describe preparation evidence, not GPU qualification. `gpu-setup
prepare` rereads the selected installation and returns a profile. `verify-bindings`,
review and the confirmed `apply` command still gate activation.

For a stopped ordinary service without a reported ControlGroup, the restricted
placement contract supports systemd 252, 255 and 259. It needs authoritative
active manager and app.slice metadata. Escaped names, templates, custom slices
and unknown versions need explicit qualified evidence.

Runtime rechecks placement and launch identity. Upgrading systemd can need
refreshing setup. Source inspection and fixture checks do not replace host
qualification.

Discovery uses fixed GET routes and disables redirects and ambient proxies. It
limits responses to 1 MiB, each probe to 3 seconds and discovery to 15 seconds.
It never starts applications, loads or unloads models, downloads files, scans
drives, or writes application or supervisor configuration.

Unsupported syntax, shell wrappers and escaped launch commands receive
actionable guidance. External adoption validates the effective unit plus
ordered contributing file hashes and pre-start commands. Setup restricts
pre-start commands to system `true`/`false` with an optional `--`. The other
allowed form is `test` with one file predicate (`-e`, `-f`, `-d`, `-r`, `-w`,
`-x`, or `-s`) and an absolute, clean path. Setup keeps originals and refuses unsupported
commands, and keeps the supported vLLM `--max-model-len` and
`--served-model-name` options. Generating a managed launch uses its distinct
restricted format.

### Upgrade and interrupted activation

Before you replace the package, use the currently activated controls to finish
or explicitly stop admitted work. Bring the deployment to stable, healthy Idle
with admission closed. Stop the proxies and prevent external workload
automation from restarting during maintenance. This is an explicit operator
decision. Package installation and setup do not do it for you.

After package replacement, the new entrypoints reject the previous activated
release before opening or migrating state. Open the setup application to
activate the new release. Changing the activated release needs a valid full
semantic version and a stable target with a strictly higher major, minor or
patch version. For example, `0.1.7-SNAPSHOT-<hash>` can activate `0.1.8` but
not `0.1.7` or another snapshot. Snapshot commit hashes do not prove upgrade
order. Reapplying the exact same release (including the same snapshot) remains
supported.

During activation:

- Setup holds both controller and exclusive proxy gates and verifies GPU release for the existing and proposed committed mappings before snapshotting state.
- Each activation keeps an integrity-checked snapshot under the private `backups/activation-*` directory. It contains previous binary checksums, profile, accepted catalog, deployment marker, state, and the exact accepted supervisor-owned unit files together. `owned-units.json` records their SHA-256 hashes. The
  private `owned-units/` directory holds their bytes. Keep the complete tuple
  for rollback.
- Reopening setup after an interruption recovers the exact pending request.
  Post-commit failures resume forward, and setup never rolls back a known committed catalog.
- Setup rejects a stale preview before maintenance starts.
- Do not delete maintenance or migration metadata to bypass a failure.

### Restore and rollback

There is no automatic downgrade. Follow the [restore procedure](OPERATIONS.md#make-a-backup-restore-and-roll-back-state).
For a managed rollback, restore the complete state, profile, ownership metadata and compatible binaries from one matching verified backup directory.

Install the matching backend and extension package. The saved `manifest.json`
records the release and SHA-256 of each saved executable. Saved binaries have
mode `0600`. Check the hashes before making a chosen recovery copy executable
with mode `0700`. Restore the matching setup metadata to these locations:

| Backup file | Private destination beneath `~/.config/gpu-workload-supervisor/` |
| --- | --- |
| `operator.json`, `catalog.json`, `ownership.json` | The corresponding files directly in this directory |
| `manifest.json` | `activated-binaries/manifest.json` |
| `gpu-mode`, `gpu-workload-proxy`, `gpu-operator`, `gpu-setup` | Corresponding files in `activated-binaries/` |

While all components remain stopped, check each `owned-units/` file against
`owned-units.json` and the matching saved catalog's launch fingerprint. Restore
these files to `~/.config/systemd/user/` with their saved unit filenames and
mode `0600`, owned by the service identity. Use only plain files from this
verified private backup. You can recreate an absent destination.

Before replacing an existing unit, check that it is a trusted regular file with
the exact fingerprint recorded by the failed deployment's accepted catalog.
Keep and investigate foreign, modified, symlinked, or untrusted files. Do
not overwrite them. Likewise, remove a unit introduced by the failed activation
only when that catalog proves its current bytes and the restored catalog no
longer references it. Never delete other user units or drop-ins.

Run `systemctl --user daemon-reload` after restoring unit files and before
reconciliation or restarting any runtime. Check that each restored owned unit
resolves to its expected file without unexpected drop-ins. Backups from before
support for owned-unit snapshots do not contain these files: get and check the
exact original bytes separately or repair forward.

While all components stay stopped, remove the failed activation's
`transaction.json`, `activation.json`, and any `owned-units-journal.json` from
the private configuration directory. Preserve them with the failed deployment
for investigation. If you cannot check the exact backup and binary pair, repair
forward.

### Remove and reinstall

Before removing the package, `gpu-setup remove-integration` removes only the
recorded reconciliation and idle-timer enablement links, stopping the recorded
idle timer before removing either link. It does not stop workload services.

Setup keeps or rejects modified or unowned integration files without
deletion. Disable the extension through GNOME Extensions. Package removal
removes package-owned binaries, extension, launcher, and units. Profiles,
workload units, models, state, audit, and backups stay private user data.
Reinstall the matching release or activate a newer one through setup.

### Continuous integration coverage

For CI coverage of the package payload and setup executable, see
[development](MAINTENANCE.md#development).

Deployment qualification must still cover real GNOME Shell 50 rendering and
session lifecycle, NVIDIA operation, dependency resolution and configured
package installation. It must also cover cross-version managed upgrade and
complete rollback and fence rotation. Successful automated checks do not close
host acceptance.

#### Installed applications that need correction

Setup distinguishes evidence of installation from qualification. Detection
inspects known executable names in standard directories and in at most 32 clean
absolute PATH directories. It also inspects `~/.local/bin`, `/opt/vllm`, its
`bin` directory and saved explicit executable selections. vLLM also checks
`~/AI/vllm` and `~/AI/vllm-supervisor`, each with `bin` and `.venv/bin`.

These bounded checks keep executable trust validation and never run programs or
recursively scan your home. For another location, choose the installed
executable directly or choose its folder. Folder inspection checks only the
application filename directly in that folder and its `bin` folder. An
unreachable default address does not mean the application is absent.

An installed application can remain blocked from adoption. Open the application
settings and select View details for every reported blocker. Runtime
executables, interpreters, scripts, search directories and data paths must
exist. You or root must own them, and they must not be world-writable.

Setup allows group write only for groups you belong to. The single-admin host policy
trusts members of your own groups. This provides no isolation from those
members. Setup checks symlink targets and every traversed ancestor.

Correct the reported path or choose a trusted installed location, then select
`Check again`. Setup does not copy programs, change permissions or ownership,
remove arguments or rewrite external units to make them qualify.

For unresolved GPU visibility, Supervisor uses its shared trusted NVIDIA
inventory. With one supported GPU, **Configure GPU** previews a drop-in and
**Apply GPU configuration** confirms it. You do not need to choose a GPU for
each application. Setup does not assume that numeric CUDA ordinals match NVIDIA
inventory indices. Multiple GPUs need supervisor-wide configuration. This flow
does not support multi-GPU scheduling.

Automatic correction supports stopped, unmanaged user services whose fragment
is directly in `~/.config/systemd/user`. It rechecks the service, launch files
and GPU inventory against the preview fingerprint. Then it creates a new
`UNIT.d/zz-gpu-workload-supervisor.conf` without replacing existing files,
reloads the user manager and reinspects the service. It never starts a workload
or edits the original unit. Setup refuses managed services and pending
activation.

A reload or validation error after writing reports that setup saved the
override. Repair and recheck before proceeding.

If automatic correction is unavailable or fails, manual recovery remains
available. **Copy reviewed override** copies the proposal only. A reported
restart-policy blocker can include `Restart=no`, which disables automatic crash
recovery. Have the service administrator review the assignments, add the
drop-in and reload the user manager. Other launch, model and trust blockers
still need correction.

`Check again` refreshes the installation and main
application card without reopening setup. Fresh qualification unlocks
`Use installation`.

Copy diagnostic report on the application page produces one bounded shareable
report across discovered installations, with application, installed and
qualified states and blocker categories. It excludes paths, service identities,
models, GPU UUIDs, endpoint values and raw errors. Local Copy details and Copy
reviewed override contain concrete local information. Review those before
sharing them.

<a id="operate-and-recover-workloads"></a>

## Use and recover workloads

Use the pinned binaries and complete [deployment configuration](DEPLOYMENT.md#deploy-the-supervisor) for every command.

### Switch workloads

After successful reconciliation, select the target with one switch command:

```sh
/PINNED/RELEASE/gpu-mode [runtime flags] -target text switch
/PINNED/RELEASE/gpu-mode [runtime flags] -target media switch
/PINNED/RELEASE/gpu-mode [runtime flags] -target idle switch
```

Replace the binary path and `[runtime flags]` with your deployment configuration. Put flags before the single command. `text` and `media` admit the selected workload after verification. `idle` keeps admission closed. Switching requires supervisor ownership. Do not start runtimes independently.

### Boot and explicit recovery

Configure user-systemd startup declaratively in the deployment repository:

1. Disable independent startup of both workload runtimes. Include timers,
   sockets, update jobs, and other activation paths that can start them outside
   controller transitions. Keep the installed units for controller use.
2. Make a reconciliation step use the pinned CLI and complete runtime flags.
   Order it after its durable state and user-systemd prerequisites are available.
3. Make successful reconciliation mandatory before starting execution proxies or any
   automation that can request workloads. Ordering alone (`After=`) is not a
   success dependency. Use a required dependency and propagate the reconciliation
   command's nonzero exit. Never prefix that command with `-` to ignore failure.
4. Keep failure latched and admission unavailable. Do not automatically invoke
   `recover`, `recover-user`, `resolve-work`, or `restore-state` from a restart
   loop. Check the recorded state and runtime evidence before explicit recovery.

Reconciliation does not resume interrupted jobs or take ownership from a user.
User-owned state requires operator inspection and `recover-user` verification.
Boot automation must not silently convert it to supervisor ownership. After
successful explicit recovery, resume only the path appropriate to the verified
owner.

A user-owned deployment must not repeatedly run supervisor reconciliation.
Return control explicitly before resuming supervisor automation.
Actual unit definitions, identities, startup targets, and deployment automation
are outside this repository.

```sh
/PINNED/RELEASE/gpu-mode [runtime flags] status
/PINNED/RELEASE/gpu-mode [runtime flags] reconcile
/PINNED/RELEASE/gpu-mode [runtime flags] recover
```

- `status` reports persisted ownership and admission plus observed active
  workload. It is not a full readiness or release check and can latch an
  observation failure for supervisor-owned state. Check the exit code as well
  as JSON.
- A successful idle `reconcile` is stable and healthy with admission closed.
  Select `text` or `media` through the controller to admit that workload. Do
  not start its runtime independently.
- Use `recover` only after repairing the cause of a supervisor-owned failure.
  Recovery first closes admission and rotates its fence. Before stopping media
  it waits up to `-drain-timeout` for already admitted work to finish. Old
  registrations keep their completion authority. Failed or canceled recovery
  stays closed with error health and unknown runtime state.
- Unfinished orphan work needs the explicit `resolve-work` procedure below
  before destructive recovery can proceed.

Runtime error messages omit untrusted command output and can omit the underlying
probe detail. Inspect the affected unit with `systemctl --user status UNIT` and
`journalctl --user -u UNIT` for service diagnostics. The supervisor does
not automatically copy suppressed probe output to the journal.

### Interrupted legacy discovery

An upgrade can find an unfinished temporary Ollama discovery session.
The supervisor blocks recovery, workload activation, and catalog changes until cleanup succeeds.
Temporary discovery cannot start new sessions.
Cleanup requires the current database schema. If the schema differs, use a compatible release to finish cleanup before the upgrade.

1. Pause external application control, including timers and other service activation paths.
2. Run the status command as the desktop account:

   ```sh
   /usr/bin/gpu-setup temporary-status
   ```

3. Read the recorded unit, invocation, session ID, and token. Stop the execution proxy before cleanup.
4. Replace `SESSION_ID` and `SESSION_TOKEN` with the values from the status result:

   ```sh
   printf '%s\n' '{"id":"SESSION_ID","token":"SESSION_TOKEN","externalControlPaused":true}' | /usr/bin/gpu-setup temporary-cleanup
   ```

Cleanup checks the saved session, launch files, and invocation before it stops the service.
It completes the session only after it confirms an inactive service and an empty cgroup.
A changed invocation or failed check keeps the gate closed. Inspect the service and retry cleanup after you correct the cause.
Do not delete the session record to bypass cleanup.

### Ownership and unfinished work

Ownership changes always need a target. Add the normal runtime flags before the command:

```sh
/PINNED/RELEASE/gpu-mode [runtime flags] -target text take-control
/PINNED/RELEASE/gpu-mode [runtime flags] -target media user-switch
/PINNED/RELEASE/gpu-mode [runtime flags] -target idle return-control
/PINNED/RELEASE/gpu-mode [runtime flags] -target idle recover-user
```

- `take-control` changes supervisor ownership to user ownership. It closes admission, rotates the fence, drains registered work, and verifies the target before committing ownership.
- `user-switch` selects `text`, `media`, or `idle` while user-owned. `return-control` selects the supervisor's workload explicitly. Both authorize terminating all current user work, including queued media work and same-target transfers. They stop both runtime units before starting the selected target.
- `switch` remains a supervisor-only command. It rejects user-owned state.
- User ownership keeps supervisor admission closed. Healthy, stable user execution bypasses lease registration only for the selected workload. The supervisor blocks execution during switching, idle, or an error state.

User execution requests hold a shared cross-process handoff lock. User switches and returns close admission, stop user runtimes and check release, then take the exclusive handoff lock. They wait up to `-drain-timeout` for forwarding handlers to exit before restarting anything. A stalled handler makes the operation fail closed. Cancel the client request or stop the proxy before recovery. All proxies sharing a state database must use this version's handoff locking before enabling ownership commands.

#### Recover after a failed transfer or switch

1. Inspect the recorded owner and runtime evidence. Failed or interrupted operations keep the last committed owner. Rollback never restarts stopped user work.
2. Repair the cause. For supervisor ownership, run `recover` with the complete runtime flags.
3. For user ownership, choose the workload to keep and check it. For example:

   ```sh
   /PINNED/RELEASE/gpu-mode [runtime flags] -target media recover-user
   ```

   Use `text` or `idle` instead when appropriate. This verifies without starting or stopping runtimes, preserves user ownership and keeps supervisor admission closed. Idle recovery verifies release under the configured policy. If verification fails, repair or explicitly stop runtimes and retry.
4. Resume only the path appropriate to the verified owner. Do not repeatedly reconcile user-owned state. `reconcile` never takes it over.

Every transfer, user switch and recovery rotates the fence and durably records source and target ownership and the outcome.

### Resolve orphaned work

Use this only after investigating work that cannot report completion, such as a submission with a lost registration token. It stops all running work.

1. Stop every `gpu-workload-proxy` for the state file and wait for shutdown. Do not use older proxies without the lifetime lock.
2. Record an incident reason and resolve the work:

   ```sh
   /PINNED/RELEASE/gpu-mode [runtime flags] -resolve-reason 'operator incident reference' resolve-work
   ```

3. Confirm success. The command requires release of all proxy lifetime locks. It closes admission and rotates the fence. It stops both runtimes. It verifies stopped units and empty workload cgroups. It then atomically marks unfinished work abandoned and records an audit event. Any stop or verification failure leaves work unfinished and admission closed.
4. After successful resolution, run `recover`, restart proxies and select the needed workload.

### Restore state

Restored databases need an explicit `/PINNED/RELEASE/gpu-mode -state /PRIVATE/DURABLE/STATE/state.db restore-state` before any proxy starts. The command needs no runtime flags and does not copy a backup. Follow the [restore procedure](OPERATIONS.md#make-a-backup-restore-and-roll-back-state) for ordering and validation. Normal restart does not rotate the fence.

<a id="retain-or-archive-audit-history"></a>

### Keep or archive audit history

Audit deletion is explicit. Ordinary proxy work retention does not remove audit
history. Set the retention period, create a consistent SQLite backup using the
[backup procedure](OPERATIONS.md#back-up-before-an-upgrade), and keep that archive
according to your audit requirements. Then run bounded batches:

```sh
/PINNED/RELEASE/gpu-mode -state /PRIVATE/DURABLE/STATE/state.db \
  -audit-before CUTOFF_RFC3339 -audit-batch 256 prune-audit
```

The cutoff and paths are placeholders. Repeat until `prunedAuditRecords` is zero.
A batch removes at most 256 audit parents (maximum configurable 1024), together
with their events and snapshots. In-progress and current-fence transitions and
transitions referencing unfinished work remain.

The command prunes restoration and work-resolution
history only with no unfinished work or live transition. It always keeps
the latest record of each kind. Resolve safety evidence through normal recovery.
Do not delete protected records manually to meet a storage target.

Archive more
often or provision more storage if unresolved evidence keeps growing.
Current-fence transitions stay pinned until a normal switch, ownership change or
recovery rotates the fence. If the workload runs unchanged for a long time, plan
archive and storage checks on your retention schedule. Do not rotate solely to prune.

Pruning holds the controller lock but does not need proxy shutdown: it changes
only eligible audit history, preserving unfinished-work evidence in one SQLite
transaction. Ctrl-C cancels a wait for the controller lock.

Deletion permits SQLite page reuse. It does not shrink the database file. Physical
reclamation needs separate offline maintenance: stop all proxies and controllers,
make a backup, then use SQLite `VACUUM` with adequate temporary disk space. Never replace
or copy a live database file or discard its WAL.

## Local operator protocol

`/usr/bin/gpu-operator` is an on-demand, one-request process for the local
account. It accepts no arguments and never listens on a socket. Its authority
is that account's existing operator authority. It does not change automation
grants. Setup and runtime control are separate entrypoints.

The process resolves the effective UID through the OS account database,
ignoring `HOME` and XDG overrides. It reads
`~/.config/gpu-workload-supervisor/operator.json` through anchored, no-follow
file descriptors. The home and directory chain must not be group- or
world-writable.

Ancestors can be root-owned. The regular profile must be
account-owned. Setup writes the profile. Runtime requests cannot change it.

The version 1 profile contains `statePath`, `activatedRelease`,
`systemctlPath`, `nvidiaSMIPath`, `gpuIndex`, optional `capacityHeadroomMiB`,
`statusTimeoutSeconds`, and `operationTimeoutSeconds`. Paths are absolute and
clean. The live catalog comes only from the accepted SQLite catalog, never an
edited configuration file. After acquiring the controller gate, the process
checks deployment activation before opening or migrating state.

### Request and response

Write exactly one compact JSON document followed by a newline, then close
stdin. The process limits input to 16 KiB and two seconds. The backend rejects
unknown, duplicate, case-aliased and trailing fields or documents. The 1-64
byte request ID consists of ASCII letters, digits, hyphen, underscore or period
and is only a correlation identifier, never an execution lease.

The backend rejects unknown request fields. The shipped client rejects unknown
response fields. Adding fields to an existing action is not automatically
backward compatible: evolve both peers together with an explicit
protocol-version change for incompatible shapes. New actions can use their own
shapes. Earlier backends reject unknown actions with `invalid_request`.

```json
{"protocolVersion":1,"requestId":"r1","action":"status"}
```

The response contains `protocolVersion`, `requestId`, and `code`. A successful response
to `status` and the transition actions additionally contains `status` with:

- `owner`, `desiredWorkload`, `activeWorkload`, `phase`, `health`, `admission`.
- `observedAt` (UTC RFC3339).
- `expected`: `incarnation`, `version`, `owner`, `configurationRevision`.
- `workloads`: bounded `{id,label}` entries including Idle.
- `capabilities`: `takeControl`, `userSwitch`, `returnControl` booleans.

Versions are canonical nonzero unsigned 64-bit decimal **strings**. Do not
parse them through JavaScript Number. IDs use the catalog's lowercase
identifier rules. Labels use the catalog's bounded display validation. Unknown
is never selectable.

Mutations copy the complete `expected` object from a fresh successful status:

```json
{"protocolVersion":1,"requestId":"r2","action":"user-switch","target":"speech","expected":{"incarnation":"opaque","version":"42","owner":"user","configurationRevision":"opaque"}}
```

`user-switch` and `activate-workload` accept a `target`. Activation excludes
`idle` (see [Demand activation](#demand-activation)). `take-control` transfers
Supervisor ownership to User while preserving the freshly verified current
workload. It drains work and verifies without lifecycle changes, including on
failure. `return-control` requires User ownership and stops work before
committing Supervisor ownership in Idle.

The store checks incarnation, version,
owner and configuration revision in the transition transaction before effects.
The backend never replays a repeated request.

### Operator settings

`get-settings` takes no `expected` and answers from durable state without
observing the runtime. A successful response contains `settings` — never
`status`, because an unobserved durable snapshot is not a fresh observation:

```json
{"protocolVersion":1,"requestId":"r3","action":"get-settings"}
```

```json
{"protocolVersion":1,"requestId":"r3","code":"ok","settings":{"policy":{"timeoutMinutes":0},"settingsRevision":"opaque"}}
```

`policy.timeoutMinutes` is 0 (Off) or 5–1440 minutes. `settingsRevision` is an opaque
concurrency token rotated on every committed write.

`set-idle-policy` carries the complete `expected` object from a fresh successful status
plus a `settings` object. Copy its `settingsRevision` from a fresh successful
**get-settings** response. Never copy it from status, which does not carry it:

```json
{"protocolVersion":1,"requestId":"r4","action":"set-idle-policy","expected":{"incarnation":"opaque","version":"42","owner":"supervisor","configurationRevision":"opaque"},"settings":{"timeoutMinutes":60,"settingsRevision":"opaque"}}
```

The store revalidates incarnation, version, owner, configuration revision and
settings revision inside the writer transaction. Settings never interrupt an
unstable state or a running transition. A committed write rotates the settings
revision and clears any armed idle deadline in the same transaction.

Without a qualified idle-evidence provider, the process rejects policy
enablement (`timeoutMinutes` nonzero) with `unavailable`. A stale settings
revision is `stale_state`, a running transition is `busy`, and an out-of-bounds
timeout is `invalid_request`. The successful response holds the updated
`settings` object only.

Both settings actions use the status-class lifetime. A mixed-version peer that
does not know these actions answers `invalid_request` with the strict error
shape, which clients treat as the idle policy being unavailable.

### Demand activation

`activate-workload` starts a catalog workload from the idle, admission-closed
supervisor state. It has the complete `expected` object from a fresh
successful status and a `target` workload (a configured profile, never `idle`),
and no `settings` field. Inside the writer transaction, the store revalidates
the operator precondition and these conditions:

- `owner=supervisor`, `phase=stable`, `health=healthy`.
- `admission=closed`, `active/desired=idle`.
- Zero unfinished admitted work.

```json
{"protocolVersion":1,"requestId":"r5","action":"activate-workload","expected":{"incarnation":"opaque","version":"42","owner":"supervisor","configurationRevision":"opaque"},"target":"text"}
```

A committed activation rotates the lease fence. The successful response additionally
carries the fresh fence for exactly one execution generation:

```json
{"protocolVersion":1,"requestId":"r5","code":"ok","status":{"owner":"supervisor","...":"..."},"leaseFence":{"incarnation":"opaque","epoch":"7"}}
```

Activation never interrupts: a running transition stays `busy`, user ownership
is `wrong_owner`, a recovery latch is `recovery_required`, a stale precondition
is `stale_state`. If the precondition is readable but the observed state
stops startup now, the response is `deferred`. Examples are an active
workload (`active != idle`) or unfinished admitted work. The response includes the
observed current `status` plus the current committed `leaseFence`.

After a lost activation response, the caller must fetch fresh `status` and
retry with that status's `expected` object: replaying the old precondition
returns `stale_state`. A deferred response to the fresh retry recovers the
committed generation. `deferred` latches nothing. The caller can retry it.
Activation uses the mutation-class lifetime.

### Inactivity policy tick

`gpu-setup idle-policy-tick` is the oneshot policy evaluation driven by the
`gpu-workload-supervisor-idle.timer` user timer every 60 seconds. Its oneshot
`gpu-workload-supervisor-idle.service` carries
`ConditionPathExists=%h/.config/gpu-workload-supervisor/operator.json`. Guided
setup enables the packaged timer link alongside the reconciliation unit, and
remove-integration removes both.

It reads the persisted `operator.json` profile and opens the profile's
configured state path and runtime. If the file is unreadable, it reports an
error. It never uses default paths as a fallback.
(`gpu-mode -state <path> idle-policy-tick` remains the explicit-path form for
diagnostics.) The tick
reads the committed settings and durable state.

An Off policy, user ownership, a non-stable or latched state, or an idle or
unknown workload no-ops after disarming any armed deadline. With no qualified
evidence provider configured, an enabled policy fails closed: the tick exits
nonzero and never idles. Qualified fresh evidence must show no queued,
reserved, running or unresolved work and no unfinished admissions.

The attestation must be no older than 120 seconds. With this evidence, the tick
commits a verified armed deadline (last activity + timeout). Once the armed
deadline elapses without newer activity, the tick drains the workload into
idle. It acquires the provider's evidence-generation fence inside the store's
writer transaction, atomically with the pending-work recheck under the
single-writer lock.

The provider atomically validates the attested generation and excludes new
queue or reservation changes until the store releases the fence after commit or
rollback. A revoked generation aborts the commit, disarms, and fails closed.
After commit, draining closes admission before the store releases the fence.

Any preemption — new activity, an admission, a state change — aborts cleanly
with exit 0 and no latch. The next tick re-verifies from fresh evidence.

Status and mutations both use the same nonblocking controller gate. `busy`
means the backend queued no request. Errors contain no status and use exactly:
`invalid_request`, `unsupported_version`, `incompatible_configuration`,
`stale_state`, `wrong_owner`, `busy`, `recovery_required`, `timeout`,
`unavailable`, `deferred`.

A `deferred` activation response additionally carries the observed current
status. Invalid request envelopes can have an empty correlation ID. Error
responses do not disclose profile paths or runtime output.

### Lifetimes and limits

Status defaults to 30 seconds, with a profile maximum of 60 seconds. Mutation
execution defaults to 15 minutes, with a maximum of 30 minutes. Existing
failure cleanup can add two minutes and durable finalization can add ten
seconds.

Response writing has a separate two-second limit and a 64 KiB cap. Recommended
client limits are 75 seconds for status and the settings actions and 33 minutes
for mutations. Calculate observation age from monotonic dispatch time.

Before admission the process detaches its session and ignores SIGHUP and
SIGPIPE. Failure to detach returns `unavailable`. The operation context is
independent of stdin and stdout.

Execution and durable finalization finish before response writing. Closing the
output reader or disabling the client cannot cancel an admitted transition.
Output failures cannot cause retries.

A lost response is an uncertain outcome: get fresh status before another
operator decision. SIGKILL, host shutdown, and session-manager logout policies
can still interrupt execution and need existing explicit recovery. Automated
subprocess tests exercise session detachment, SIGHUP and closed output pipes.
They do not qualify real GNOME Shell restart or logout behavior.

<a id="gnome-shell-extension-client"></a>

### Client for the GNOME Shell extension

`clients/gnome/gpu-workload-supervisor@local` implements the GNOME Shell 50
extension shipped with the local operator. It uses native SystemIndicator and
QuickMenuToggle controls and invokes only `/usr/bin/gpu-operator`, with one
bounded JSON request per process. The client needs no proxy, gateway, Job
Broker, listener or persistent UI service.

The local OS account supplies operator authority. Ownership confirmation
expresses intent, not verified human presence.

The committed toggle is OFF under Supervisor ownership and ON under User
ownership. Take Control confirms preserving the verified current workload. Stop
and Return confirms stopping work and returning Supervisor ownership in Idle.

User workload selection is immediate. Selecting the active workload does
nothing. Normal menus show configured labels.

Details retains owner, requested and active workloads, phase, health, admission
and observation freshness bounded to thirty seconds after dispatch.

- The client never optimistically changes committed ownership.
- Pending calls, stale observations, errors and recovery requirements disable
  mutations.
- One call can be outstanding. Status polling backs off from five to sixty
  seconds and never overlaps a mutation.
- Freshness expires thirty seconds after dispatch, using monotonic time.
- Versions stay decimal strings. The client rejects retired enable
  generations, stale decisions and lower same-incarnation versions.
- Disconnects need fresh status. The client never replays mutations.
- Client read deadlines exceed the backend's maximum operation plus cleanup and
  finalization and input and output budgets. The numbers are in [Lifetimes and
  limits](#lifetimes-and-limits).
- A client deadline leaves the result uncertain.
- Disable cancels local reads and removes UI resources, without killing the
  backend.
- The backend must independently preserve admitted operations across Shell
  restart.
- There is no recovery control in the extension.

Run contract, model, and mocked lifecycle tests with
`npm test --prefix clients/gnome`. Run the native bounded-stream smoke test with `gjs -m
clients/gnome/tests/transport.gjs.js`. The latter needs GJS and is not a Shell
qualification. Before enablement, qualify a real GNOME 50 session:

- Check the keyboard and visual access to the toggle, menu and Details.
- Cancel and confirm both ownership dialogs.
- Check the third-workload selection, active no-op and pending repeated clicks.
- Check the stale and faulted observations.
- Check the disable/re-enable behavior and Shell restart during admitted work.

Confirm backend survival and later fresh status with the actual deployment.
Headless tests do not give this evidence. Packaging alone does not enable the
extension or qualify a distribution.

The project does not support browser automation: there is no dashboard or
browser client, and no transport grants browser callers the local operator's
authority.

## Optional requester ingress

The proxy can authenticate individual requesters and authorize exact workflows
before recording demand or activating a workload. This is opt-in. Auto-idle
stays Off. The packaged operator still rejects enablement until deployment
qualification proves that direct execution on the runtime cannot bypass this
ingress. Loopback binding and credentials alone do not prove that isolation.

### Configuration

Add `-requester-policy-file /absolute/private/requesters.json` to the existing
`gpu-workload-proxy` adapter-mode invocation. Keep separate execution and
completion credential files. The proxy state path must match its account's
accepted operator profile. You do not need a new daemon, timer or remote
listener.

The policy file must pass the same private-file ownership, permissions and
no-symlink checks as credential files. The proxy limits it to 1 MiB and loads
it once. Restart the proxy after changing it. Example structure:

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

Each workflow must name a configured execution route. The workload is the
proxy's configured workload. Requester credentials must be distinct from every
other requester and both adapter credentials.

IDs are 1-64 ASCII letters, digits, dots, underscores or hyphens. Limits: 128
requesters, 128 workflows each, 16 MiB request bodies. This first version
authorizes immutable payloads.

Changing inputs or whitespace requires approving the new body digest. A
caller-provided workflow name is not approval for an arbitrary graph.

Requester mode excludes read-only and passthrough routes. The shared execution
credential cannot bypass requester policy. Only the completion credential can
call the existing terminal endpoint.

Requesters never receive completion tokens. The proxy forwards neither requester
credentials nor workflow headers to runtimes.

### Requests and cancellation

Send the approved body to its execution route with:

- `X-Workload-Requester-Credential`: the requester's credential.
- `X-Workload-Workflow`: the approved workflow ID.
- `X-Request-ID`: a unique demand ID (or the configured request-ID header).

The gateway supplies the current lease fence. The proxy rejects unauthorized
requests, mismatched bodies, duplicate identity headers, query strings, encoded
bodies and trailers before recording demand. It never stores credentials or
bodies. Durable evidence contains requester and workflow IDs and the body
digest.

A healthy Supervisor-owned Idle workload activates through the existing typed
operator action, waits for readiness and admits only with the resulting fence.
A different active workload or transition returns HTTP 409 `demand_queued` and
keeps durable demand. It never interrupts work or forces an active workload
into Idle.

The caller keeps its payload and explicitly retries the same request when the
supervisor can accept it. There is no background replay worker.

To cancel a queued demand, send **DELETE to the same execution path**, with the
same credential, workflow, request ID and approved body. Success returns 204.
Cancellation cannot stop a reservation or admitted work. Cancel deferred
requests before removing their credentials or workflow policy.

Once reserved or admitted, duplicate requests return `demand_conflict` rather
than replaying execution. A process crash, disconnect or ambiguous dispatch
leaves evidence unresolved. Verified operator recovery must quiesce proxies,
check runtimes stopped and rotate the fence before resolving it.

No timer silently expires unfinished demand. Backup restoration retires reserved
demand and its claims atomically with the old lease. Queued intent stays
available for explicit retry or cancellation.

Existing completed-work retention also removes old canceled or terminal
demand records. The store limits queued and reserved demand together to
4096. It limits retained records to 65536. Reaching capacity rejects new work.

### Durable evidence

Queue insertion, reservation and work registration are SQLite transactions.
Admission consumes the reservation claim in the same transaction as the existing
native or ComfyUI work registration. Queued and reserved demand invalidates
pending idle deadlines.

The atomic idle decision checks that demand. Admitted work remains pending until
trusted terminal evidence or verified operator recovery completes it. Existing
native streaming and ComfyUI reconciliation contracts still apply.

This evidence does not attest to external jobs that bypass the gateway. The
deployment has no qualified `EvidenceProvider`, and auto-idle enablement remains
unavailable. Host runtime isolation and terminal and lifecycle qualification are
a distinct deployment step. Fixture tests do not satisfy that requirement.

<a id="back-up-restore-and-roll-back-state"></a>

## Make a backup, restore, and roll back state

<a id="back-up-before-an-upgrade"></a>

### Make a backup before an upgrade

Record the pinned binary version and checksum and complete runtime and proxy configuration
alongside the backup. Stop the controller automation and prevent new execution.
Finish or explicitly stop running jobs, then stop every proxy and workload runtime
using the state file. Keep these components from restarting during maintenance.

Create a consistent SQLite backup using SQLite's backup mechanism or a verified
consistent snapshot. Keep the complete database, including audit and migration
metadata. Do not copy only the main `.db` file from a live WAL database. Protect
backup contents and the durable state directory with the service identity's private
permissions.

Check the backup integrity and keep the previous binary and configuration
before the new binary or proxy opens state and applies migrations.

Before upgrading, select each media profile's stop policy explicitly through its
catalog `adapter`. The validator no longer accepts the previous implicit choice. The
`systemd` adapter stops the unit and is the only qualified media release path.
The compatibility `media-unload` adapter remains unsupported for live media
because the supervisor cannot currently check release, even with a configured release
endpoint. See the [deployment policy](DEPLOYMENT.md#deploy-the-supervisor).
`restore-state` and `prune-audit` need no media stop policy.

An ordinary restart against the unchanged database is not a restore. After an
upgrade of supervisor-owned state, reconcile with the new pinned binary and
matching runtime flags before restarting proxies. For user-owned state, inspect
the runtime and explicitly check it with `recover-user`. `reconcile` rejects user
ownership. Use `return-control` only when the operator intends to resume supervisor
ownership. Follow the [ownership and recovery guidance](OPERATIONS.md#ownership-and-unfinished-work)
and validate the deployment before accepting work.

### Restore a backup

The lease incarnation in a SQLite backup can be older than the live database. A restored fence is unsafe until `restore-state` rotates it. This procedure is for an operator restoring a consistent backup, not for an ordinary process restart.

1. Stop every execution proxy, controller automation, and workload runtime using this state database. Wait for admitted jobs to finish or stop them. Prevent another process from starting these components during the restore. The controller lock serializes commands, and `restore-state` also refuses to run while a proxy holds its lifetime lock. Neither lock stops proxies or runtimes or protects the earlier backup replacement.
2. Install a consistent SQLite backup at the configured state path while all processes that use it remain stopped. Use a backup made with SQLite's backup mechanism or another consistent snapshot. Do not copy only the main `.db` file from a live WAL database. Do not leave WAL or SHM files from another database alongside the replacement. Keep the state file in a private directory owned by the service identity.
3. Prepare the restored state, before starting any proxy:

   ```sh
   /PINNED/RELEASE/gpu-mode -state /PRIVATE/DURABLE/STATE/state.db restore-state
   ```

   The database must be an initialized supervisor database. The command rejects an empty placeholder. The command needs no runtime flags and does not copy a backup. On success it prints the closed state with a new incarnation and atomically:

   - Takes supervisor ownership.
   - Sets the active workload to unknown.
   - Closes admission.
   - Marks restored active work abandoned.
   - Invalidates in-progress transitions.
   - Records the restore in audit history.

   Failed work and transition history remain available. If it fails, keep all components stopped and resolve the error before retrying.

   If `restore-state` reports unfinished training, keep all components stopped. If the job ID is unknown, use Python 3's standard-library SQLite reader against the restored state path. Run it as the service identity. This read-only query prints only IDs and recovery categories, never training payloads or tokens:

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

   Explicitly recover each listed ID before continuing. As the service identity, use a text editor to create a private regular file with mode `0600` at `/PRIVATE/DURABLE/STATE/training-job-id.txt`. Save exactly one ID (without the tab and category), followed by one newline. Preserve all spaces and do not add quotes or escapes. Paste into the editor, not a terminal input prompt: valid IDs can exceed the terminal's line-length limit.

   Read the file as data. If the file contains exactly one nonempty line, run recovery. Keep the variable expansion quoted:

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

   Use the matching runtime configuration and trainer bindings from the restored database's accepted catalog. Runtime commands do not accept `-catalog`. A `reserved job` blocks `restore-state`. Before you retry it, recovery must succeed and you must check recursive cgroup and GPU release. A `preparing claim` does not reserve GPU work and does not block `restore-state`. Recovery records it as `interrupted` without stopping a trainer, and the query excludes finished jobs and interrupted claims.

   If the query fails or lists no reserved job despite the unfinished-training error, keep components stopped. Investigate the database path and schema. Do not guess an ID or edit state. After successful recovery, retry `restore-state`, then continue with step 4. Do not bypass the unfinished-training guard.

   [Training recovery](TRAINING.md#commands) never resumes or relaunches training: it preserves an already persisted terminal outcome, otherwise records `interrupted`. The backup alone does not prove completion. If recovery fails, keep proxies stopped and resolve the runtime or configuration error before retrying.

4. Keep the proxies stopped. Run the same pinned `gpu-mode` with the `reconcile` command and full runtime flags. Include the same `-state` path. It must finish with a stable, healthy state. If it reports a failure, leave the proxies stopped and use `recover` only after checking the runtime state. If you need an active workload, start it through the controller.
5. Start the proxies only after successful reconciliation. Clients must get the new fence. The proxy rejects requests with a fence from the backup.

`restore-state` acquires the exclusive proxy lifetime lock before opening the database and holds it through completion. Proxies keep their shared locks until in-flight handlers finish. This enforces proxy shutdown only. Operators must still stop all runtimes before replacing the database.

Repeat `restore-state` after every later installation of a backup. Do not run it on a normal restart: reopening an existing database preserves the current lease fence. A restore cannot stop work that already executes outside the proxy, so stopping execution before replacing the database is part of the safety boundary.

### Roll back a release

There is no automatic database downgrade. Never point an older binary at the live
post-upgrade database only because you can replace the executable. You must check the schema and
behavior compatibility for that exact binary and database pair.
The current migration guard rejects a newer schema. Compatible schema
numbers alone do not prove compatible fencing, handoff locking, or completion
semantics. Do not edit migration metadata to force an older binary to open state.

For a supported rollback, stop and block all components as above. Preserve the
failed deployment's database for investigation. Select the already verified
binary and configuration plus its matching pre-upgrade backup. Managed setup backups
also keep accepted owned unit bytes and hashes.

Restore them and refresh the
user manager as described in [Desktop restore and rollback](OPERATIONS.md#restore-and-rollback),
protecting modified or foreign unit files. Follow every restore
step above, including `restore-state`, using a binary verified to support that
backup and the restore protocol. Do not mix older proxies without the required
handoff and lifetime locks with a newer controller. Reconcile and check closed-state
recovery before resuming execution.

Refresh client fences after restoration.
Rollback does not recover work and audit updates newer than the backup.
If the previous release cannot safely do this procedure, leave admission
closed and repair forward with a compatible release.
