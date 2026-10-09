# Local GNOME desktop installation

[Documentation](README.md) | [Operator protocol](OPERATOR.md) | [Restore procedure](RESTORING.md)

The Debian package contains the compatible CLI, proxy, one-shot operator backend,
GNOME extension, and native Gtk4/Libadwaita setup application. GNOME Shell **50** is
the initial target. The package requires GJS, Gtk4, Libadwaita, and systemd. Linux
amd64 and arm64 artifacts are built; these are build targets, not a claim that a
particular Debian-family distribution or desktop/GPU combination is qualified.
Real Shell 50 session, logout/login, NVIDIA, upgrade, and package-manager acceptance
must pass on the deployment host before enabling production controls.

## Install and configure

Install the verified release `.deb` with the distribution package manager, then
open **GPU Workload Setup** as the desktop account. Package installation installs
files only: there are no maintainer scripts opening user databases, enumerating
sessions, stopping workloads, or enabling services for arbitrary users.

Choose applications, choose models only when relevant, then finish. Setup
presents ComfyUI, Ollama, llama.cpp and vLLM once each, with models grouped under
their application. It selects one unambiguous recognized installation; additional
installation controls are deferred. ComfyUI workflows select their own models.
Applications and models must already exist.

Review the ready-to-finish summary after active jobs finish, then select
**Finish setup** to confirm activation. ComfyUI closes when you switch to another
workload. Labeled gears expose service, endpoint, launch and resource settings
for inspection or supported overrides. Automatic and manual
configuration use the same validation and activation safeguards.

Setup refuses stale configuration, edited integration files, live workloads,
unfinished transitions and unsafe ownership/state relocation. Discovery and
preview never start applications or load models. Reconciliation is enabled for
future logins without `--now`; it is a oneshot, not a UI daemon.

The extension may require logout/login before GNOME discovers a new system-wide
installation. Enable **GPU Workload Supervisor** in GNOME Extensions afterward.
An unsupported Shell version or session is rejected by setup. Shell 50 discovery
and visuals require real-session qualification; headless tests do not establish it.

The effective account's OS home determines the private profile location:
`~/.config/gpu-workload-supervisor/operator.json`. Environment HOME/XDG overrides
do not select runtime configuration. The accepted workload catalog and its opaque
revision live in SQLite; `catalog.json` is only an owned setup/backup artifact.
Ordinary controls never adopt edits to that mirror. Setup exposes `discover`,
`probe`, `prepare`, `fingerprint`, `drafts`, `save-drafts`, `verify-bindings`, `validate`,
`apply`, `temporary-status`, `temporary-discover`, `temporary-cleanup`, `reconcile`
and `remove-integration`; request/response commands use a
strict versioned JSON protocol over stdin/stdout. The desktop application
supplies these requests and renders the concrete preview.

## Manage workloads in setup

Open **Manage workloads** from GPU Control in GNOME's top-right Quick Settings
menu. The packaged setup application is also available in the application list.

1. Select application cards, then **Continue**.
2. Choose relevant models beneath each application, then **Continue**. ComfyUI
   skips model selection. Discovery refresh reads metadata without startup.
3. Review the summary, finish active jobs and switch to Idle before **Finish setup**.
   Setup verifies current evidence and uses the existing activation safeguards.

**Save selections for later** keeps choices for later without making them selectable workloads.
Missing or unsupported installations offer a location/address fallback and
explain what evidence is missing. Custom wrappers and ambiguous configuration
require a supported launch or validated Advanced configuration; setup does not
guess ownership from a healthy endpoint.

An application's gear opens its settings at the beginning of the view. **Back**
returns to the originating screen and gear while keeping selections. Candidate
choices identify the service or address and its status; the selected candidate's
full identity is also available in selectable text. An unreachable default
address does not establish an installation. Installed/stopped services, missing
locations, missing model choices and inspection failures have separate guidance;
optional **Technical details** retain the backend cause.

For installed Ollama, llama.cpp or vLLM without an existing supported service,
choose **Choose installed executable…** in the application's settings, then
**Continue** to choose an existing model name, file or directory. The next
**Continue** validates the executable and model and previews a Supervisor-managed
launch. **Finish setup** explicitly permits Supervisor to start and stop it; the
preview does not start it. **Advanced** separates managed launch options from
adopting an already configured external service. Cancelling a file chooser keeps
the previous selection. ComfyUI continues to use its existing service configuration.

Existing recognized service files and supported drop-ins are adopted without
rewriting their launch options. Optional gear settings retain the managed-launch
and explicit-binding workflows. Applications, environments and models are never
installed, updated or downloaded by setup.

llama.cpp and vLLM external services retain the model pinned by their launch;
choosing another model does not rewrite their service. Ollama models can share
one verified instance. New or edited adopted groups receive read-only launch and
cgroup preflight, followed by complete activation and GPU release checks. The backend
`render-owned` command returns a preview only; Apply creates the managed launch
files. See [WORKLOADS.md](WORKLOADS.md). Model identity is checked again before
GPU admission.

Configured workloads can be renamed or edited. **Edit application** reopens
the same setup flow and retains the workload identity and unrelated settings. **Remove from supervisor** changes
only supervisor configuration. Active/referenced workloads cannot be removed;
finish their work and switch to Idle first. Applications, models, workflows and
external configuration are preserved.

**Set up later** closes the window without applying configuration. **Save selections for later**
first if you want to keep new choices. A stale-draft error requires reopening
Manage workloads before retrying. Interrupted activation resumes its recorded
configuration, with editing disabled until it completes.

Native GTK keyboard/accessibility checks and real GPU qualification remain part
of the GNOME/package host acceptance tracked in
[issue #146](https://github.com/mickey-kras/gpu-workload-supervisor/issues/146)
and [issue #148](https://github.com/mickey-kras/gpu-workload-supervisor/issues/148).
Automated widget and protocol tests do not qualify the host installation.

## Application discovery

`gpu-setup discover` returns saved setup state plus `applications` candidates for
ComfyUI, Ollama, llama.cpp and vLLM. It probes four default loopback ports and
inspects recognizable user-service launch metadata. Default endpoint observations remain separate from service ownership. A recognized
service derives its endpoint and launch identity from loaded metadata and its
unchanged supported launch file. Existing catalog profiles remain unchanged.
Discovery errors are reported separately from an empty inventory.

For a non-default endpoint or explicit reference:

```sh
printf '%s' '{"app":"ollama","endpoint":"http://127.0.0.1:11434"}' | gpu-setup probe
printf '%s' '{"app":"llama.cpp","reference":"/models/model.gguf","referenceKind":"model-file"}' | gpu-setup probe
```

`app` accepts `comfyui`, `ollama`, `llama.cpp`, `vllm`. Select exactly one
`endpoint` or `reference`. `referenceKind` accepts `application`, `configuration`,
`model-file`, `model-directory`. Endpoint values are loopback HTTP(S) origins,
without credentials, path, query or fragment. References must be absolute clean
paths. Ollama, llama.cpp and vLLM `application` selections permit executable
aliases only when the target and every link hop pass executable trust checks.
Configuration, model, directory and ComfyUI references accept only regular files
or directories and reject symbolic links. File contents are not executed or
inspected, and directories are not enumerated.

| Application | Read-only observations |
| --- | --- |
| ComfyUI | `/system_stats` version; workflows select models, no model picker |
| Ollama | `/api/version`, available `/api/tags`, loaded `/api/ps`; remote entries marked `non-local` |
| llama.cpp | Native `/models` with explicit statuses, or older `/v1/models` served identities; simple launch model references |
| vLLM | `/version` and `/v1/models`; same-root aliases grouped; simple launch model references |

Missing optional APIs report `unsupported` or unknown loaded state. Unreachable
instances never become successful empty inventories. A verified stopped unit is
`not-running`; a failed endpoint connection is `unreachable`. Saved references
survive either condition. Served identities do not establish a disk inventory or
distinct physical models. File selection does not establish compatibility.

All candidates retain `lifecycleControl: "unverified"`. `recognized` and
`configurationStatus` describe preparation evidence, not GPU qualification.
`gpu-setup prepare` rereads the selected installation and returns a profile;
`verify-bindings`, review and confirmed Apply still gate activation.

For a stopped ordinary service without a reported ControlGroup, the restricted
placement contract supports systemd 252, 255 and 259 and requires authoritative
active manager/app.slice metadata. Escaped names, templates, custom slices and
unknown versions require explicit qualified evidence. Runtime rechecks placement
and launch identity; upgrading systemd can require refreshing setup. Source
inspection and fixture checks do not replace host qualification.

Stopped Ollama may require an existing model name when native inventory is
unavailable. Metadata-only setup remains available. An initialized compatible
Supervisor in healthy, closed Idle can offer a separate temporary inventory
action with explicit startup/GPU-use consent and paused external application
control. It starts only the qualified stopped service, keeps admission closed,
and returns models after verified stop and GPU/cgroup release. Cancellation
requests cleanup; interrupted or failed cleanup remains recorded and blocks
ordinary controls until explicit guarded cleanup succeeds. It never stops an
application that was already running or a replaced service invocation. Native
model readiness remains a separate runtime check before admission.

Discovery uses fixed GET routes, disables redirects and ambient proxies, limits
responses to 1 MiB, and bounds each probe to 3 seconds and discovery to 15 seconds.
It never uses llama.cpp reload/routed autoload requests, starts applications,
loads/unloads models, downloads files, scans drives, or writes application or
supervisor configuration. Unsupported syntax, shell wrappers and escaped launch
commands receive actionable guidance. External adoption validates the effective
unit plus ordered contributing file hashes and pre-start commands. Pre-start
commands are restricted to system `true`/`false` with an optional `--`, or `test`
with one file predicate (`-e`, `-f`, `-d`, `-r`, `-w`, `-x`, or `-s`) and an absolute,
clean path; originals are preserved and unsupported commands are refused.
Supported llama.cpp parallel, continuous-batching, flash-attention and MTP options are
preserved; generating a managed launch uses its separate restricted format.

## Upgrade and interrupted activation

Before replacing the package, use the currently activated controls to finish or
explicitly stop admitted work and bring the deployment to stable, healthy Idle
with admission closed. Stop proxies and prevent external workload automation from
restarting during maintenance. This is an explicit operator decision; package
installation and setup do not perform it for you.

After package replacement, the new entrypoints reject the previous activated
release before opening or migrating state. Open setup to activate the new release.
Changing the activated release requires a valid full semantic version and a
stable target with a strictly higher major/minor/patch version. For example,
`0.1.7-SNAPSHOT-<hash>` can activate `0.1.8`, but cannot activate `0.1.7` or another
snapshot. Snapshot commit hashes do not establish upgrade order. Reapplying the
exact same release (including the same snapshot) remains supported.

During activation:

- Setup holds both controller and exclusive proxy gates and verifies the existing
  and proposed committed mappings have released the GPU before snapshotting state.
- Each activation retains an integrity-checked snapshot under the private
  `backups/activation-*` directory: previous binary checksums, profile, accepted
  catalog, deployment marker, and state together. Keep it for rollback.
- Reopening setup after an interruption recovers the exact pending request;
  post-commit failures resume forward, and a known committed catalog is never
  rolled back.
- A stale preview is rejected before maintenance starts.
- Do not delete maintenance or migration metadata to bypass a failure.

## Restore and rollback

There is no automatic downgrade. Follow every step in [RESTORING.md](RESTORING.md).
For a managed rollback, restore one matching verified backup directory's complete
state, profile, ownership metadata and compatible binary set together, and install
the matching backend/extension package. The saved `manifest.json` records the
release and SHA-256 of each saved executable; saved binaries have mode `0600`.
Verify the hashes before making a chosen recovery copy executable with mode `0700`.
Restore the matching setup metadata to these locations:

| Backup file | Private destination beneath `~/.config/gpu-workload-supervisor/` |
| --- | --- |
| `operator.json`, `catalog.json`, `ownership.json` | The corresponding files directly in this directory |
| `manifest.json` | `activated-binaries/manifest.json` |
| `gpu-mode`, `gpu-workload-proxy`, `gpu-operator`, `gpu-setup` | Corresponding files in `activated-binaries/` |

While all components remain stopped, remove the failed activation's
`transaction.json` and `activation.json` from the private configuration
directory; preserve them with the failed deployment for investigation. If the
exact backup/binary pair cannot be verified, repair forward.

## Remove and reinstall

Before removing the package, `gpu-setup remove-integration` removes only the
recorded reconciliation and idle-timer enablement links. It stops the recorded
idle timer before removing either link; it does not stop workload services.
Modified or unowned integration files are preserved or rejected without deletion.
Disable the extension through GNOME Extensions.
Package removal removes package-owned binaries, extension, launcher, and units;
profiles, workload units, models, state, audit, and backups remain private user
data. Reinstall the matching release or activate a newer one through setup.

## Continuous integration coverage

CI coverage of the package payload and setup executable is described in
[DEVELOPMENT.md](DEVELOPMENT.md).

Real GNOME Shell 50 rendering/session lifecycle, NVIDIA operation, dependency
resolution and configured package installation, cross-version managed upgrade,
and complete rollback/fence rotation still require deployment qualification.
Use the [pinned candidate qualification checklist](SETUP-QUALIFICATION.md);
successful automated checks do not close host acceptance.
