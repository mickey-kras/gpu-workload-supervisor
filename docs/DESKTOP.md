# Local GNOME desktop installation

[Documentation](README.md). [Operator protocol](OPERATOR.md). [Restore
procedure](RESTORING.md).

The Debian package contains the compatible CLI, proxy, one-shot operator
backend, GNOME extension, and native Gtk4/Libadwaita setup application. GNOME
Shell **50** is the initial target. The package requires GJS, Gtk4, Libadwaita,
and systemd.

CI builds Linux amd64 and arm64 artifacts. These build targets do not qualify a
particular Debian-family distribution or desktop and GPU combination. Real
Shell 50 session, logout and login, NVIDIA, upgrade, and package-manager
acceptance must pass on the deployment host before enabling production
controls.

## Install and configure

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

## Manage workloads in setup

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

See [WORKLOADS.md](WORKLOADS.md). The controller checks model identity again
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

## Application discovery

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
| vLLM | `/version` and `/v1/models`. Same-root aliases grouped. Model references in simple launches. With an API key, models are `unknown` |

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
`--served-model-name` options.

A vLLM launch must also use the hardened settings in
[WORKLOADS.md](WORKLOADS.md#hardened-vllm-launch). Generating a managed launch
uses its distinct restricted format with these settings. The model folder must
contain `chat_template.jinja`.

## Upgrade and interrupted activation

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
  for rollback. The snapshot does not hold the API key files of vLLM. After a
  rollback, run setup again to create missing key files.
- Reopening setup after an interruption recovers the exact pending request.
  Post-commit failures resume forward, and setup never rolls back a known committed catalog.
- Setup rejects a stale preview before maintenance starts.
- Do not delete maintenance or migration metadata to bypass a failure.

## Restore and rollback

There is no automatic downgrade. Follow every step in
[RESTORING.md](RESTORING.md). For a managed rollback, restore one matching
verified backup directory's complete state, profile, ownership metadata and
compatible binary set together.

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

## Remove and reinstall

Before removing the package, `gpu-setup remove-integration` removes only the
recorded reconciliation and idle-timer enablement links, stopping the recorded
idle timer before removing either link. It does not stop workload services.

Setup keeps or rejects modified or unowned integration files without
deletion. Disable the extension through GNOME Extensions. Package removal
removes package-owned binaries, extension, launcher, and units. Profiles,
workload units, models, state, audit, and backups stay private user data.
Reinstall the matching release or activate a newer one through setup.

## Continuous integration coverage

For CI coverage of the package payload and setup executable, see
[DEVELOPMENT.md](DEVELOPMENT.md).

Deployment qualification must still cover real GNOME Shell 50 rendering and
session lifecycle, NVIDIA operation, dependency resolution and configured
package installation. It must also cover cross-version managed upgrade and
complete rollback and fence rotation. Successful automated checks do not close
host acceptance.

### Installed applications that need correction

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
