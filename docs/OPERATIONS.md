# Deploy and use

- [Install and manage desktop workloads](#local-gnome-desktop-installation)
- [Switch, recover or change ownership](#use-and-recover-workloads)
- [Make a backup, restore or roll back](#make-a-backup-restore-and-roll-back-state)
- [Operator protocol](#local-operator-protocol), [requester ingress](#optional-requester-ingress), [idle policy](#demand-activation-and-inactivity-idle-plan)

## Local GNOME desktop installation

The Debian package includes CLI, proxy, one-shot operator, GNOME extension and Gtk4 and Libadwaita setup. It requires GJS, Gtk4, Libadwaita and systemd. GNOME Shell 50 is the initial target.

CI builds for Linux amd64 and arm64 do not qualify distributions or hosts. Before production controls, qualify real Shell 50 rendering, keyboard and accessibility, session restart, logout and login, NVIDIA, dependencies and package installation. Also qualify cross-version managed upgrade, complete rollback and fence rotation. Headless widget and protocol tests do not qualify the host: see [#146](https://github.com/mickey-kras/gpu-workload-supervisor/issues/146) and [#148](https://github.com/mickey-kras/gpu-workload-supervisor/issues/148).

### Install and configure

Install a verified release `.deb` with the distribution package manager. Open **GPU Workload Setup** as the desktop account. Installation only adds files: maintainer scripts never inspect sessions, open user databases, stop workloads or enable arbitrary users' services.

Applications, environments and models must already exist. Setup never installs, updates or downloads them. Other installation controls remain future work. ComfyUI workflows choose their models.

Setup rejects unsupported Shell versions or sessions. A new system-wide extension can need logout and login for discovery. Then enable **GPU Workload Supervisor** in GNOME Extensions. Setup enables future-login reconciliation without `--now`: a oneshot, not a UI daemon.

The OS home of the effective account fixes `~/.config/gpu-workload-supervisor/operator.json`. Ambient HOME and XDG overrides cannot choose runtime configuration. SQLite holds the accepted catalog and opaque revision. Setup owns `catalog.json` for setup and backup only. Ordinary controls ignore mirror edits.

Setup exposes `discover`, `probe`, `correct-gpu`, `prepare`, `fingerprint`, `render-owned`, `drafts`, `save-drafts`, `verify-bindings`, `validate`, `apply`, `reconcile`, `idle-policy-tick`, `remove-integration`. Request and response commands use strict versioned stdin and stdout JSON. The desktop supplies requests and shows concrete previews.

### Manage workloads in setup

Open **Manage workloads** from GPU Control in GNOME Quick Settings or the packaged application list.

1. Select application cards. Select **Continue**. Setup shows ComfyUI and vLLM once each and selects an unambiguous recognized installation.
2. Choose one model per application, except ComfyUI. Discovery refresh reads metadata without startup. Select **Continue**.
3. Review the summary, finish active jobs, switch to Idle, confirm **Finish setup**. This permits Supervisor starts and stops. Switching away closes ComfyUI.

Discovery and preview never start applications, load models or change services. Automatic and manual configuration share validation. Activation rejects stale configuration or previews, edited integration files, live workloads, unfinished transitions, unsafe ownership or state relocation and blocked installations.

New or edited adopted services receive read-only launch and cgroup preflight and complete activation and GPU-release checks. `render-owned` previews. `apply` creates managed files. The controller rechecks model identity before admission.

| Control or condition | Behavior |
| --- | --- |
| **Save selections for later** | Saves choices without selectable workloads. `Set up later` closes without applying. Save first to keep new choices |
| Gear, **Back** | The gear opens service, endpoint, launch and resource settings at the view's beginning. **Back** returns to the originating screen and gear while keeping selections |
| Candidate | Shows the service or address and its status. Full identity appears as wrapped selectable text. `Use installation` checks and reviews. Only **Finish setup** activates |
| Missing or unsupported installation | Offers location or address fallback and missing-evidence guidance. Custom wrappers or ambiguity need supported launches or validated Advanced configuration. Healthy endpoints do not prove ownership |
| Inspection failure | Separate guidance for stopped services, missing paths or models and unreachable addresses. **View details** opens Configuration details, which shows the reported problems. Use **Copy details** to copy them or `Check again` to repeat inspection |
| Multiple blockers | Independent checks report together. Dependent checks need reinspection after evidence repair |
| **Edit application** | Reopens the same flow. Retains workload identity and unrelated settings. Supports rename and edit |
| **Remove from supervisor** | Configuration only. Rejects active or referenced workloads. Finish work and switch Idle. Preserves applications, models, workflows and external configuration |
| Stale draft | Reopen Manage workloads before retrying |
| Interrupted activation | Resumes its recorded configuration. Editing stays disabled until completion |

For installed vLLM without a supported service, choose **Change installation…**, then **Choose installed executable…**. In **Advanced settings**, choose **Review changes** and an existing model directory. **Continue** validates both and previews a managed launch. Canceling a chooser retains its previous selection. Advanced settings separates common fields, **Resource checks** and **Launch details**. Launch details separates managed options from external adoption.

Recognized external services and drop-ins keep launch options. ComfyUI retains its service configuration. Optional gears keep managed-launch and explicit-binding workflows. An external vLLM service pins its model: each different model needs its own service.

Supported external `ExecStart`, `Environment`, `WorkingDirectory` path tokens can begin with `%h`. Resolution requires one effective-UID entry in trusted root-owned local `/etc/passwd` matching loaded systemd configuration. Setup rejects missing or NSS-only records, ambient HOME fallback and other expansions. Expanded paths undergo literal-path ownership, permission and symlink checks. See [configured workloads](INTEGRATION.md#configured-workloads).

### Application discovery

`gpu-setup discover` returns saved state and ComfyUI and vLLM `applications` candidates. It checks two default loopback ports and recognizable user-service metadata. Loaded metadata plus unchanged supported launch files define service endpoint and identity. Endpoint observations do not prove ownership. Existing profiles remain unchanged. Errors differ from empty inventories.

```sh
printf '%s' '{"app":"vllm","endpoint":"http://127.0.0.1:8000"}' | gpu-setup probe
printf '%s' '{"app":"vllm","reference":"/models/model","referenceKind":"model-directory"}' | gpu-setup probe
```

`app`: `comfyui` or `vllm`. Select exactly one `endpoint` or `reference`. `referenceKind`: `application`, `application-directory`, `configuration`, `model-directory`.

| Input or observation | Contract |
| --- | --- |
| Endpoint | Loopback HTTP(S) origin. No credentials, path, query or fragment |
| Reference | Absolute clean path. Configuration, model, directory or ComfyUI references need regular files or directories without symlinks |
| vLLM application alias | Target and every link hop must pass executable trust checks |
| vLLM application-directory | Known filenames directly in selected directory and `bin`. No recursion, content inspection or execution |
| ComfyUI | `/system_stats` version. No model picker |
| vLLM | `/version`, `/v1/models`. Groups same-root aliases. Model references from simple launches |
| Optional API absent | `unsupported` or unknown loaded state |
| Verified stopped unit / failed connection | `not-running` / `unreachable`. Never successful empty inventories. Retains saved references |

Served identities prove neither disk inventory nor distinct physical models. Selecting files proves no compatibility. Candidates keep `lifecycleControl: "unverified"`. `recognized`/`configurationStatus` mean preparation, not GPU qualification. `prepare` rereads the installation and returns a profile. `verify-bindings`, review and confirmed `apply` still gate activation.

Stopped ordinary services without ControlGroup support restricted placement on systemd 252, 255 and 259 with authoritative active-manager and app.slice metadata. Escaped names, templates, custom slices and unknown versions need explicit qualified evidence. Runtime rechecks placement and launch identity. Systemd upgrades can need setup refresh.

Discovery uses fixed GET routes, without redirects or ambient proxies, 1 MiB responses, 3-second probes and a 15-second total. It never starts applications, loads or unloads models, downloads files, scans drives or writes configuration.

External adoption checks effective units, ordered contributing-file hashes and pre-start commands. Allowed commands: system `true`/`false` with optional `--`, or `test` with one `-e`, `-f`, `-d`, `-r`, `-w`, `-x` or `-s` predicate and absolute clean path. Originals and vLLM `--max-model-len`/`--served-model-name` remain. Unsupported syntax, wrappers and escaped launches get guidance. Managed launches use a separate restricted format.

### Upgrade and interrupted activation

Before package replacement, use active controls to finish or stop admitted jobs, reach stable healthy Idle with closed admission, stop proxies and prevent external restart. Installation and setup do not do this operator decision.

New entrypoints reject the previous activated release before opening or migrating state. Activate through setup. Changed releases need valid full semantic versions and a stable target with strictly higher major, minor or patch version. `0.1.7-SNAPSHOT-<hash>` can activate `0.1.8`, not `0.1.7` or another snapshot. Hashes prove no order. Exact-release reapplication, including snapshots, remains supported.

Activation holds controller and exclusive-proxy gates and verifies GPU release for existing and proposed mappings before snapshotting. Private `backups/activation-*` contains integrity-checked previous binaries and checksums, profile, accepted catalog, deployment marker, state and exact accepted owned-unit bytes. `owned-units.json` records SHA-256 hashes. `owned-units/` holds bytes. Keep this complete rollback tuple.

Setup rejects stale previews before maintenance. Reopening resumes the exact pending request. Post-commit failures proceed forward, never undo a committed catalog. Never delete maintenance or migration metadata to bypass failure.

### Restore and rollback

Use the [full restore procedure](#make-a-backup-restore-and-roll-back-state), keeping components stopped. Restore state, profile, ownership metadata and compatible binaries from one verified matching backup. Install the matching backend and extension packages. `manifest.json` records release and executable SHA-256. Check the saved mode-`0600` binaries before making recovery copies executable (`0700`).

| Backup | Destination beneath `~/.config/gpu-workload-supervisor/` |
| --- | --- |
| `operator.json`, `catalog.json`, `ownership.json` | Same names |
| `manifest.json` | `activated-binaries/manifest.json` |
| `gpu-mode`, `gpu-workload-proxy`, `gpu-operator`, `gpu-setup` | Same names under `activated-binaries/` |

Check the plain private `owned-units/` files against `owned-units.json` and saved catalog launch fingerprints. Restore saved filenames to `~/.config/systemd/user/`, service-owned, mode `0600`. You can recreate absent destinations.

Replace the existing trusted regular units only if their fingerprints match the failed deployment's accepted catalog. Preserve and investigate foreign, modified, symlinked or untrusted files. Remove the failed-activation units only when that catalog proves current bytes and the restored catalog no longer references them. Never delete other units or drop-ins.

Run `systemctl --user daemon-reload` before reconciliation or runtime restart. Check each restored unit's expected file and absence of unexpected drop-ins. Older backups without owned-unit snapshots need separately verified original bytes, or forward repair.

While stopped, preserve the failed deployment's `transaction.json`, `activation.json`, `owned-units-journal.json` for investigation, then remove them from private configuration. If you cannot check the exact backup and binary pair, repair forward.

### Remove and reinstall

Before package removal, run `gpu-setup remove-integration`: it stops the recorded idle timer, then removes only recorded timer and reconciliation enablement links. It never stops workloads or deletes modified or unowned integration files. Disable the extension through GNOME Extensions.

Package removal deletes package-owned binaries, extension, launcher and units. Private profiles and workload units, models, state, audit and backups remain. Reinstall the matching release or activate a newer release through setup.

### Continuous integration coverage

See [development](MAINTENANCE.md#development) for package and setup CI. Automated checks do not replace the [host acceptance requirements](#local-gnome-desktop-installation).

#### Installed applications that need correction

Detection checks known executables in standard directories, at most 32 clean absolute PATH directories, saved selections, `~/.local/bin` and `/opt/vllm` (including `bin`). vLLM also checks `~/AI/vllm` and `~/AI/vllm-supervisor`, including `bin`/`.venv/bin`. It never runs programs or recursively scans home. Elsewhere, select an executable or folder. Folder checks cover only known filenames directly and in `bin`. Unreachable defaults do not mean absent applications.

For blockers, open application settings and **View details**. Executables, interpreters, scripts and search directories and data paths must exist, belong to you or root and lack world-write permission. Group write requires your membership. This single-admin policy does not isolate your group members. Checks include symlink targets and all traversed ancestors.

Correct or select trusted paths, then `Check again`. Setup never copies programs, changes ownership or permissions, removes arguments or rewrites external units to qualify them.

GPU correction uses the shared trusted NVIDIA inventory. With one supported GPU, **Configure GPU** previews and **Apply GPU configuration** confirms a drop-in. No per-application selection. Numeric CUDA ordinals need not match NVIDIA indices. Multiple GPUs need supervisor-wide configuration, not multi-GPU scheduling.

Automatic correction requires a stopped unmanaged user service with its fragment directly in `~/.config/systemd/user`. It rejects managed services or pending activation. After rechecking service, launch files and GPU inventory against the preview fingerprint, it creates `UNIT.d/zz-gpu-workload-supervisor.conf`, reloads and reinspects. It replaces no file, edits no original unit and starts nothing. Post-write reload or validation errors report the saved override: repair and recheck.

Manual fallback: **Copy reviewed override** copies only the proposal. Ask the service administrator to review, install and reload it. A restart-policy blocker can propose `Restart=no`, disabling automatic crash recovery. Correct remaining launch, model and trust blockers. `Check again` refreshes installation and main card in place. Fresh qualification unlocks `Use installation`.

**Copy diagnostic report** gives a bounded cross-installation report of application, installed and qualified states and blocker categories. It excludes paths, service identities, models, GPU UUIDs, endpoints and raw errors. Local **Copy details**/**Copy reviewed override** contain concrete local data: review before sharing.

<a id="operate-and-recover-workloads"></a>

## Use and recover workloads

Use pinned binaries and complete [deployment flags](DEPLOYMENT.md#deploy-the-supervisor). Replace the placeholders. Put flags before the single command. Never start runtimes independently.

### Switch workloads

```sh
/PINNED/RELEASE/gpu-mode [runtime flags] -target text switch
/PINNED/RELEASE/gpu-mode [runtime flags] -target media switch
/PINNED/RELEASE/gpu-mode [runtime flags] -target idle switch
```

After reconciliation, Supervisor-only `switch` verifies before admitting `text`/`media`. `idle` keeps admission closed.

### Boot and explicit recovery

Define user-systemd startup in the deployment repository:

1. Disable independent runtime activation, including timers, sockets and update jobs. Keep units for controller use.
2. Run pinned reconciliation with complete flags after durable-state and user-systemd prerequisites.
3. Make successful reconciliation mandatory before proxies or workload automation. `After=` alone is insufficient: use required dependencies, propagate nonzero exit, never ignore failure with `-`.
4. Keep failures latched and admission unavailable. Never automatically loop `recover`, `recover-user`, `resolve-work` or `restore-state`. Inspect the state and runtime evidence first.

Reconciliation never resumes jobs or takes User ownership. Inspect User state and check with `recover-user`. Return control explicitly before Supervisor automation. Do not repeatedly reconcile User state. Actual units, identities, targets and automation are deployment-specific.

```sh
/PINNED/RELEASE/gpu-mode [runtime flags] status
/PINNED/RELEASE/gpu-mode [runtime flags] reconcile
/PINNED/RELEASE/gpu-mode [runtime flags] recover
```

| Command | Evidence and outcome |
| --- | --- |
| `status` | Persisted ownership and admission and observed active workload. Not full readiness or release proof. Can latch Supervisor observation failures. Check the exit code and JSON |
| Idle `reconcile` | Success means stable, healthy and closed. Select text or media through the controller |
| `recover` | Repair Supervisor failure first. Closes admission, rotates fence, waits up to `-drain-timeout` before stopping media. Old registrations keep completion authority. Failure or cancellation leaves closed admission, error health and unknown runtime |

Resolve orphaned work before destructive recovery. For suppressed probe and command-output details, inspect `systemctl --user status UNIT` and `journalctl --user -u UNIT`. The supervisor does not copy suppressed output to journals.

### Interrupted legacy discovery

If temporary Ollama discovery remains unfinished, the supervisor blocks recovery, activation and catalog changes. The supervisor refuses new sessions. Cleanup needs the current schema: otherwise use a compatible release before upgrading.

1. Pause external control, timers and other activation paths. As the desktop account, run:

   ```sh
   /usr/bin/gpu-setup temporary-status
   ```

2. Read unit, invocation, session ID and token. Stop the proxy. Substitute the returned values:

   ```sh
   printf '%s\n' '{"id":"SESSION_ID","token":"SESSION_TOKEN","externalControlPaused":true}' | /usr/bin/gpu-setup temporary-cleanup
   ```

Cleanup validates session, launch files and invocation before stopping. Completion requires inactive service and empty cgroup. Changed invocation or failed checks keep the gate closed: inspect, repair, retry. Never delete the session record.

### Ownership and unfinished work

```sh
/PINNED/RELEASE/gpu-mode [runtime flags] -target text take-control
/PINNED/RELEASE/gpu-mode [runtime flags] -target media user-switch
/PINNED/RELEASE/gpu-mode [runtime flags] -target idle return-control
/PINNED/RELEASE/gpu-mode [runtime flags] -target idle recover-user
```

CLI ownership commands need a target and complete runtime flags.

| Command | Contract |
| --- | --- |
| `take-control` | Supervisor → User. Closes admission, rotates fence, drains registrations and verifies target before ownership commit |
| `user-switch` | User-owned selection of text, media or idle |
| `return-control` | Explicitly selects Supervisor workload |
| `switch` | Rejects User ownership |

`user-switch`/`return-control` authorize terminating all User work, including queued media and same-target transfers. Both runtimes stop before target startup. Stable healthy User execution bypasses leases only for the selected workload. Admission stays closed. Switching, idle or error blocks execution.

User forwarding holds a shared cross-process handoff lock. A User switch or return closes admission, stops runtimes and checks release. It then acquires the exclusive lock before restarting anything. It waits up to `-drain-timeout` for handlers. Stalled handlers fail closed. Cancel requests or stop proxies before recovery.

Every proxy sharing state must support this handoff locking.

Every transfer, User switch and recovery rotates the fence and durably records source and target ownership and outcome.

#### Recover after a failed transfer or switch

Inspect the committed owner and runtime evidence. Failure retains the last owner, and rollback never restarts User work. Repair the cause. Supervisor owner: `recover` with complete flags. User owner: select the workload to preserve:

   ```sh
   /PINNED/RELEASE/gpu-mode [runtime flags] -target media recover-user
   ```

Use text or idle when appropriate. `recover-user` verifies without starting or stopping runtimes, preserves User ownership and closed admission. Idle verifies configured release policy. Repair or explicitly stop runtimes after failure, then retry. Resume only the verified owner's path. Never repeatedly reconcile User state.

### Resolve orphaned work

For investigated work without completion authority, such as lost registration tokens, this procedure stops all running work.

1. Stop every state-file proxy and wait for shutdown. Do not use older proxies without lifetime locks.
2. Record an incident reason:

   ```sh
   /PINNED/RELEASE/gpu-mode [runtime flags] -resolve-reason 'operator incident reference' resolve-work
   ```

3. Confirm success. First, all proxies must release their lifetime locks. The command closes admission, rotates the fence, stops both runtimes and checks empty cgroups. It then atomically abandons unfinished work and records an audit event. Stop or verification failure leaves unfinished work and closed admission.
4. Run `recover`, restart proxies, select the workload.

### Restore state

Before any proxy starts on restored state, follow [backup restoration](#restore-a-backup), including `restore-state`. Ordinary restart preserves its fence.

<a id="retain-or-archive-audit-history"></a>

### Keep or archive audit history

Audit deletion is explicit. Work retention never removes it. Set the retention period and archive consistent [SQLite backups](#back-up-before-an-upgrade), then run batches:

```sh
/PINNED/RELEASE/gpu-mode -state /PRIVATE/DURABLE/STATE/state.db \
  -audit-before CUTOFF_RFC3339 -audit-batch 256 prune-audit
```

Replace the cutoff and path placeholders. Repeat until `prunedAuditRecords=0`. Default batches remove at most 256 parents (configurable maximum 1024), plus events and snapshots. Keep in-progress or current-fence transitions and those referencing unfinished work. Prune restoration and work-resolution history only when neither unfinished work nor live transitions remain. Always keep each kind's latest record.

Use normal recovery for protected evidence. Never manually delete it or rotate fences solely to prune. Unchanged workloads keep current-fence history until a normal switch, ownership change or recovery rotates the fence. Schedule archives and storage checks. Archive more or add storage if unresolved evidence grows.

Pruning holds the controller lock, not proxy shutdown, and preserves unfinished evidence in one SQLite transaction. Ctrl-C cancels lock waits. Deletion reuses pages without shrinking files. For physical reclamation: stop proxies and controllers, make a backup, then use SQLite `VACUUM` with enough temporary space. Never copy or replace live databases or discard WAL.

<a id="back-up-restore-and-roll-back-state"></a>

## Make a backup, restore, and roll back state

Restore changes lease identity. Ordinary restart does not. Keep all affected components stopped throughout replacement and recovery.

<a id="back-up-before-an-upgrade"></a>

### Make a backup before an upgrade

Record the pinned binary version and checksum and complete runtime and proxy configuration. Stop the controller automation, block new execution, finish or stop jobs, then stop every proxy and runtime sharing state. Prevent maintenance restarts.

Use SQLite backup or a verified consistent snapshot, including all audit and migration metadata. Never copy only a live WAL database's `.db`. Protect backup and state directories with service-private permissions. Check the integrity. Keep previous binaries and configuration before new processes open or migrate state.

Before upgrading, [select an explicit media policy](DEPLOYMENT.md#select-an-explicit-media-policy). `restore-state` and `prune-audit` need no media stop policy.

After upgrading Supervisor state, reconcile with new pinned binary and matching flags before proxies. For User state, inspect and `recover-user`. Reconcile rejects it. `return-control` requires explicit intent. Validate the deployment before work. Follow [ownership recovery](#ownership-and-unfinished-work).

### Restore a backup

A backup's incarnation can predate live state. `restore-state` must rotate it before any proxy starts, after every backup installation, never ordinary restart.

1. Stop every proxy, controller automation and runtime sharing state. Finish or stop admitted jobs and prevent restart. Controller and proxy lifetime locks serialize commands, not earlier file replacement or external runtime stopping.
2. Install a consistent backup at the configured private service-owned state path. Never copy live `.db` alone or leave another database's WAL or SHM files alongside it.
3. Prepare restored state:

   ```sh
   /PINNED/RELEASE/gpu-mode -state /PRIVATE/DURABLE/STATE/state.db restore-state
   ```

The initialized supervisor database cannot be an empty placeholder. No runtime flags or backup copying occur. Success prints closed state and a new incarnation. Atomic changes: Supervisor ownership, unknown active workload, closed admission, abandoned restored active work, invalidated transitions and an audit record. Failed work and transition history remains. On failure keep everything stopped and repair before retrying.

For unfinished training with unknown IDs, run this read-only Python 3 standard-library query as the service identity. Output contains only IDs and categories, never payloads or tokens:

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

Recover each ID explicitly. In a text editor, create private regular mode-`0600` `/PRIVATE/DURABLE/STATE/training-job-id.txt` as the service identity. Store exactly one ID plus newline, preserving spaces, without category, tab, quotes or escapes. Do not paste into terminal prompts: IDs can exceed terminal line limits. Read as data with quoted expansion:

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

Use restored accepted-catalog runtime configuration and trainer bindings. Runtime commands reject `-catalog`.

| Query category | Recovery |
| --- | --- |
| `reserved job` | Blocks restore. Before retrying, complete recovery and check recursive cgroup and GPU release |
| `preparing claim` | Reserves no GPU, does not block restore. Recovery records `interrupted` without stopping a trainer |

The query excludes finished jobs and interrupted claims.

Failed query or no reserved job despite the error: keep stopped, investigate path and schema. Never guess IDs, edit state or bypass the guard. Recovery preserves persisted terminal outcomes, otherwise records `interrupted`. It never resumes or relaunches training. Backups prove no completion. Repair runtime and configuration failures, recover, retry `restore-state`.

4. Keep proxies stopped. Run pinned `reconcile` with full matching flags and the same `-state`. Continue only after stable, healthy success. Failure: inspect the runtime before explicit `recover`. Start any needed workload through the controller.
5. Start the proxies. Clients get the new fence. Proxies reject backup fences.

`restore-state` holds the exclusive proxy lifetime lock before database opening through completion. Proxies hold shared locks until handlers finish. This proves proxy shutdown only: operators must stop runtimes before replacement, including execution outside proxies.

### Roll back a release

No automatic downgrade. Check the exact binary and database schema and behavioral compatibility before older binaries open state. The guard rejects newer schemas. Matching schema numbers prove no fencing, handoff or completion compatibility. Never edit migration metadata.

Stop and block components as above. Preserve the failed database for investigation. Select verified binaries and configuration and matching pre-upgrade backup, including managed owned-unit bytes and hashes. Follow [desktop restoration](#restore-and-rollback), protecting foreign or modified units and refreshing the user manager. Complete every restore step, including `restore-state`, with a binary qualified for that backup and protocol. Never mix proxies that lack required handoff or lifetime locks with newer controllers.

Reconcile, check closed-state recovery and refresh client fences before execution. Rollback loses post-backup work and audit updates. If the previous release cannot do this safely, keep admission closed and repair forward.

## Local operator protocol

`/usr/bin/gpu-operator` accepts no arguments and never listens on a socket. It handles one on-demand request per process under the local account's authority, without changing automation grants. Setup is separate.

Effective-UID OS lookup ignores HOME and XDG. Anchored no-follow descriptors read `~/.config/gpu-workload-supervisor/operator.json`. Its directory chain and home must not be group- or world-writable. Ancestors can be root-owned. The regular profile must be account-owned. Only setup writes it.

Version 1 fields: `statePath`, `activatedRelease`, `systemctlPath`, `nvidiaSMIPath`, `gpuIndex`, optional `capacityHeadroomMiB`, `statusTimeoutSeconds`, `operationTimeoutSeconds`. Paths are absolute and clean. Only accepted SQLite supplies the live catalog. After acquiring the controller gate, the process checks activation before opening or migrating state.

### Request and response

Send one compact JSON document, newline, then close stdin: maximum 16 KiB and two seconds. Reject unknown, duplicate, case-aliased or trailing fields or documents. Request IDs use 1–64 ASCII letters, digits, hyphen, underscore and period bytes for correlation, never leases.

```json
{"protocolVersion":1,"requestId":"r1","action":"status"}
```

```json
{"protocolVersion":1,"requestId":"r2","action":"user-switch","target":"speech","expected":{"incarnation":"opaque","version":"42","owner":"user","configurationRevision":"opaque"}}
```

Responses contain `protocolVersion`, `requestId`, `code`. Successful status and transitions add `status`:

| Field | Content |
| --- | --- |
| State | `owner`, `desiredWorkload`, `activeWorkload`, `phase`, `health`, `admission` |
| `observedAt` | UTC RFC3339 |
| `expected` | `incarnation`, `version`, `owner`, `configurationRevision` |
| `workloads` | Bounded `{id,label}` list including Idle. Catalog lowercase IDs and bounded display labels. Never selectable Unknown |
| `capabilities` | Boolean `takeControl`, `userSwitch`, `returnControl` |

Versions are canonical nonzero unsigned 64-bit decimal strings, never JavaScript Number. Mutations copy fresh successful status's complete `expected`. The transition transaction revalidates all four fields before effects. Requests never replay.

`user-switch`/`activate-workload` accept targets. Activation excludes `idle`. Operator `take-control` drains and verifies without lifecycle changes, even on failure. It transfers ownership from Supervisor to User. It preserves the current workload. `return-control` requires User ownership, stops work, then commits Supervisor Idle.

Backend and client reject unknown request and response fields. Evolve peers together. Incompatible shapes need protocol-version changes. New actions can have separate shapes. Older backends return `invalid_request`.

Status and mutations share the nonblocking controller gate: `busy` queues nothing. Error codes: `invalid_request`, `unsupported_version`, `incompatible_configuration`, `stale_state`, `wrong_owner`, `busy`, `recovery_required`, `timeout`, `unavailable`, `deferred`. Errors omit status except deferred activation. Invalid envelopes can have empty correlation IDs. Errors expose neither profile paths nor runtime output.

### Operator settings

`get-settings` needs no `expected` or runtime observation. Success returns durable `settings`, never fresh `status`:

```json
{"protocolVersion":1,"requestId":"r3","action":"get-settings"}
```

```json
{"protocolVersion":1,"requestId":"r3","code":"ok","settings":{"policy":{"timeoutMinutes":0},"settingsRevision":"opaque"}}
```

```json
{"protocolVersion":1,"requestId":"r4","action":"set-idle-policy","expected":{"incarnation":"opaque","version":"42","owner":"supervisor","configurationRevision":"opaque"},"settings":{"timeoutMinutes":60,"settingsRevision":"opaque"}}
```

`policy.timeoutMinutes`: 0 (Off) or 5–1440 minutes. `settingsRevision` is an opaque token rotated on every committed write.

`set-idle-policy` combines fresh status's complete `expected` with settings revision from fresh **get-settings**, never status. Its writer transaction revalidates both revisions plus incarnation, version and owner, rotates settings revision and clears armed deadlines. It never interrupts unstable state or transitions. Success returns only updated settings.

Enablement without qualified evidence returns `unavailable`. Stale settings → `stale_state`, running transition → `busy`, invalid timeout → `invalid_request`. Both actions use status lifetimes. Older peers' strict `invalid_request` means idle policy unavailable.

### Demand activation

`activate-workload` takes fresh complete `expected`, a configured non-idle target and no settings. Writer-transaction requirements:

- `owner=supervisor`, `phase=stable`, `health=healthy`.
- `admission=closed`, `active/desired=idle`, no unfinished admitted work.

```json
{"protocolVersion":1,"requestId":"r5","action":"activate-workload","expected":{"incarnation":"opaque","version":"42","owner":"supervisor","configurationRevision":"opaque"},"target":"text"}
```

```json
{"protocolVersion":1,"requestId":"r5","code":"ok","status":{"owner":"supervisor","...":"..."},"leaseFence":{"incarnation":"opaque","epoch":"7"}}
```

Success rotates and returns `leaseFence` for exactly one execution generation after readiness and admission verification. Lifetime is mutation-class. Activation preserves mutual exclusion and never interrupts or preempts active work.

| Condition | Response |
| --- | --- |
| Running transition | `busy` |
| User ownership | `wrong_owner` |
| Recovery latch | `recovery_required` |
| Stale precondition | `stale_state` |
| Readable precondition, startup blocked by active workload or unfinished work | `deferred` with observed status and committed fence. No latch |

After lost responses, fetch fresh status and retry its `expected`. Old preconditions return `stale_state`. A fresh deferred retry recovers the committed generation.

Active work waits with orchestration through busy or deferred responses. Orchestration keeps queued jobs and payloads until activation. Adapters use the fresh fence. Stale registrations fail. There is no remote control facade or implicit activation.

### Inactivity policy tick

Every 60 seconds, user `gpu-workload-supervisor-idle.timer` runs oneshot `gpu-setup idle-policy-tick` through `gpu-workload-supervisor-idle.service`. The service requires `ConditionPathExists=%h/.config/gpu-workload-supervisor/operator.json`. Guided setup enables timer and reconciliation links. Remove-integration removes both.

The tick reads the persisted profile's runtime and state path and committed settings and state. Unreadable profiles fail without fallback paths. Explicit diagnostics: `gpu-mode -state <path> idle-policy-tick`.

Off, User ownership, unstable or latched state or idle or unknown workload disarms deadlines and does nothing. Enabled without qualified evidence fails closed and nonzero. Fresh evidence must exclude queued, reserved, running and unresolved work and unfinished admissions.

An attestation at most 120 seconds old arms `last activity + timeout`. On expiry without new activity, the tick drains to Idle. Inside the single-writer transaction, provider-generation fencing and pending-work rechecks are atomic with state-version and lease-fence checks. Registrations without terminal evidence count as unfinished.

The provider validates generation and excludes queue or reservation changes through commit or rollback. Revocation aborts commit, disarms and fails closed. Committed draining closes admission before fence release.

Any new activity, admission or state change aborts with exit 0 and no latch. The next tick fetches fresh evidence.

### Lifetimes and limits

| Operation | Limit |
| --- | --- |
| Backend status | Default 30 seconds, maximum 60 seconds |
| Backend mutation execution | Default 15 minutes, maximum 30 minutes |
| Failure cleanup | Up to two extra minutes |
| Durable finalization | Up to ten extra seconds |
| Response writing | Separate two-second timeout and 64 KiB size cap |
| Client status and settings | Recommended timeout: 75 seconds |
| Client mutations | Recommended timeout: 33 minutes |

Measure the observation age from monotonic dispatch. Before admission, detach the process session and ignore SIGHUP and SIGPIPE. Detachment failure returns `unavailable`. Context is independent of stdin and stdout. Execution and finalization precede response writing. Closed readers or disabled clients cannot cancel admitted transitions, and failed output never triggers retries.

Lost response means uncertain outcome: fetch fresh status before deciding. SIGKILL, shutdown and logout policies can interrupt and need explicit recovery. Subprocess tests cover detachment, SIGHUP and closed pipes, not real Shell restart and logout.

<a id="gnome-shell-extension-client"></a>

### Client for the GNOME Shell extension

`clients/gnome/gpu-workload-supervisor@local` ships native Shell 50 SystemIndicator and QuickMenuToggle controls. Each bounded JSON request invokes only `/usr/bin/gpu-operator`. No proxy, gateway, Job Broker, listener or persistent UI service. OS-account authority applies. Confirmation proves intent, not human presence.

| UI or state | Contract |
| --- | --- |
| Toggle | Committed OFF=Supervisor, ON=User. Never optimistic |
| Take Control | Confirms preservation of verified current workload |
| Stop and Return | Confirms stopping work and Supervisor Idle |
| User selection | Immediate. Active workload is a no-op. Menus use configured labels |
| Details | Owner, requested and active workloads, phase, health, admission, freshness |
| Pending, stale, error or recovery | Disable mutations. No extension recovery control |
| Calls | One outstanding. Status backs off 5–60 seconds, never overlaps mutation |
| Freshness | Expires 30 seconds after monotonic dispatch |
| Version checks | Decimal strings. Reject retired enable generations, stale decisions, lower same-incarnation versions |
| Disconnect or deadline | Fresh status required. Uncertain outcome, never mutation replay |
| Disable | Cancel reads and remove UI resources without killing backend |

Client deadlines exceed maximum backend operation, cleanup, finalization and input and output budgets. Backend preservation of admitted operations across Shell restart is independent.

Run `npm test --prefix clients/gnome` for contract, model and mock lifecycle tests. `gjs -m clients/gnome/tests/transport.gjs.js` needs GJS and tests bounded streams. Neither qualifies Shell. In a real GNOME 50 session, check:

- Keyboard and visual access to the toggle, menu and Details.
- Cancel and confirm both ownership dialogs.
- Third-workload selection, active no-op and repeated pending clicks.
- Stale and faulted observations.
- Disable/re-enable and Shell restart during admitted work.

Confirm backend survival and later fresh status. Packaging neither enables the extension nor qualifies distributions.

The project has no browser automation, dashboard or browser client. No transport grants browser callers local-operator authority.

## Optional requester ingress

Opt-in requester authentication authorizes exact workflows before demand and activation. Auto-idle stays Off. Packaged enablement requires deployment proof against direct runtime bypass. Credentials and loopback alone prove no isolation.

### Configuration

Add `-requester-policy-file /absolute/private/requesters.json` to the existing proxy adapter invocation. Keep separate execution and completion credentials. Use the account's accepted operator state path. No new daemon, timer or remote listener.

The once-loaded policy uses credential-file private ownership, permission and no-symlink checks, maximum 1 MiB. Restart after changes.

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

Workflows name configured execution routes on the proxy workload. Requester credentials differ from each other and both adapter credentials. IDs: 1–64 ASCII letters, digits, dots, underscores and hyphens. Limits: 128 requesters, 128 workflows each, 16 MiB bodies.

Authorization covers immutable exact bytes. Changed inputs or whitespace need approved digests, not just workflow names. Requester mode excludes read-only and passthrough routes. Shared execution credentials cannot bypass policy. Only completion credentials reach terminal endpoints.

Requesters never receive completion tokens. Runtimes receive neither requester credentials nor workflow headers.

### Requests and cancellation

Send approved bodies to execution routes with:

- `X-Workload-Requester-Credential`: requester credential.
- `X-Workload-Workflow`: approved workflow ID.
- `X-Request-ID`: unique demand ID, or configured request-ID header.

Gateway supplies current fence. Before demand, reject unauthorized or mismatched bodies, duplicate identity headers, queries, encoded bodies and trailers. Store no bodies or credentials. Durable evidence contains requester and workflow IDs and body digest.

Healthy Supervisor Idle activates through the typed operator, waits readiness and admits using its resulting fence. Another active workload or transition returns HTTP 409 `demand_queued` and retains demand, without interrupting work or forcing Idle. Callers keep payloads and explicitly retry. No replay worker.

Cancel queued demand with **DELETE to the same execution path**, same credential, workflow and request ID and approved body. Success 204. Cancellation does not stop reserved or admitted work. Cancel deferred requests before deleting credentials or workflow policy.

Reserved or admitted duplicates return `demand_conflict`, never replay. Crash, disconnect or ambiguous dispatch remains unresolved. Operator recovery must quiesce proxies, prove stopped runtimes and rotate fences. No timer expires unfinished demand. Restore atomically retires reservations and claims with the old lease. Queued intent permits explicit retry or cancellation.

Completed-work retention also removes old canceled or terminal demand. Maximum queued+reserved=4096. Retained=65536. Capacity rejects new work.

### Durable evidence

SQLite transactions cover queue, reservation and registration. Admission consumes claims atomically with native or ComfyUI registration. Queued or reserved demand invalidates idle deadlines and participates in atomic idle checks. Admitted work needs trusted terminal evidence or verified recovery. Native streaming and ComfyUI reconciliation contracts remain.

Evidence excludes external bypass jobs. No deployment-qualified `EvidenceProvider` exists: enablement remains unavailable. Qualify runtime isolation plus terminal and model lifecycle separately. Fixtures cannot prove this.

## Demand activation and inactivity idle plan

[#144](https://github.com/mickey-kras/gpu-workload-supervisor/issues/144) implements durable policy and settings, demand activation and a 60-second timer. Policy defaults Off. Packaged entrypoints lack qualified evidence: enablement returns `unavailable`, evaluation fails closed. Keep Off for initial desktop deployment.

### Placement

One backend loop beside the transition controller uses durable evidence and existing typed operations. The loop never runs in clients or GNOME Shell. It adds no lifecycle engine, authority or runtime-specific logic. Disabling the UI or locking the session does not affect behavior.

### Required before enablement

Qualify an orchestration `EvidenceProvider`, queued, reserved, running and unresolved evidence, runtime-bypass prevention and native terminal and model lifecycle on the deployment host. Contracts are not adapters. Fixture coverage does not qualify the host. Production isolation for requester ingress remains unqualified. Integrate desktop settings and deadlines only with backend capability and fresh evidence.

<a id="activation-ingress"></a>

### Activation

Use [typed activation](#demand-activation) for preconditions, readiness checks, fences and deferred work.

### Idle transition

The [policy tick](#inactivity-policy-tick) makes the fenced inactivity decision and aborts on intervening admission.

### Evidence input contract

Use only orchestration attestation: job-identity sets for queued, reserved, running and unresolved work. Unresolved maps to `PendingWork` retaining completion authority. Use `ResolveUnfinishedWork` only for verified operator recovery, never policy cleanup.

Pull `Attest` every evaluation. `AcquireFence` validates generation and excludes queue and reservation changes through the transaction. Invalidation is not fresh evidence. Timestamp age over 120 seconds (`MaxAttestationAge`, `internal/supervisor/policy.go`) is missing evidence. Unreachable, outage, uncertain, partial or missing attestations forbid Idle. Timer source: `packaging/gpu-workload-supervisor-idle.timer`.

<a id="ownership-transfer-while-pending-idle-is-armed"></a>

### Ownership transfer with an armed idle deadline

Take-control and return-control cancel and disarm deadlines. Recompute only from fresh post-transfer activity. User ownership exits policy. Never idle User work.

### Activity definition

Activity: admissions and verified terminal completion. Exclude UI and health polling, keyboard and session input, GPU use and VRAM. Loaded-but-unused means inactive. A disconnect without terminal evidence means unresolved work.

### Settings

The [settings protocol](#operator-settings) persists timeout minutes or Off with revision checks. The proposed 60-minute enabled default needs host measurement first. Restart derives deadlines from durable last activity, never process-local timers.

<a id="status-surface-work-item"></a>

### Status interface

`control.State` holds policy and deadline read fields. `get-settings` returns committed settings, ordinary `status` does not. Desktop presentation remains follow-up. Show only backend-verified pending deadlines. No savings estimate or live countdown. Any countdown only renders that deadline locally.

### Boundaries

Never reclaim User ownership or automatically recover latches. Latches block policy. No firmware, clock, power-limit or suspend changes. Host identities, routes, measurements and installations remain private deployment configuration.

### Acceptance

Test exact-timeout requests, cold and busy activation, mixed workloads, streaming and async completion, and long jobs.

Also test disconnects, ambiguous completion, adapter outages, stale attestations, pending-deadline restarts, armed ownership transfers, rejected requests and latches.

Record the host loaded-unused and idle power and activation latency privately before savings claims or enabled defaults.
