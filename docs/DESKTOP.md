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

Setup discovers existing systemd user services without adopting them. Add each
workload explicitly with its stable ID, display label, service, cgroup path,
loopback health URL, measured VRAM requirement, and login policy. Applications
and models must already exist. Adopt existing workload units, or use the backend's
supervisor-owned launch configuration for supported native model runtimes.
New workloads default to stop-to-idle at reconciliation; retain is an explicit choice. Live media unload
remains subject to the backend's fail-closed release restrictions.

Review **Validate and preview**, explicitly confirm a quiescent activation, then
apply. Setup validates the backend catalog and executable configuration. It refuses
user-owned or edited integration files, stale catalog revisions, live workloads,
unfinished work/transitions, and ownership/state relocation. It never silently
stops or starts workloads or changes current ownership. The reconciliation unit is
enabled for future logins without `--now`; it is a oneshot, not a UI daemon.

The extension may require logout/login before GNOME discovers a new system-wide
installation. Enable **GPU Workload Supervisor** in GNOME Extensions afterward.
An unsupported Shell version or session is rejected by setup. Shell 50 discovery
and visuals require real-session qualification; headless tests do not establish it.

The effective account's OS home determines the private profile location:
`~/.config/gpu-workload-supervisor/operator.json`. Environment HOME/XDG overrides
do not select runtime configuration. The accepted workload catalog and its opaque
revision live in SQLite; `catalog.json` is only an owned setup/backup artifact.
Ordinary controls never adopt edits to that mirror. Setup exposes `discover`,
`probe`, `fingerprint`, `drafts`, `save-drafts`, `verify-bindings`, `validate`,
`apply`, `reconcile` and `remove-integration`; request/response commands use a
strict versioned JSON protocol over stdin/stdout. The desktop application
supplies these requests and renders the concrete preview.

## Manage workloads in setup

Open **Manage workloads** from GPU Control in GNOME's top-right Quick Settings
menu. The packaged setup application is also available in the application list.

1. Choose ComfyUI, Ollama, llama.cpp or vLLM and select **Add workload**.
2. Choose a detected instance. **Refresh discovery** reads its existing inventory;
   it never starts an application or loads a model.
3. Choose a native model, or an explicit model file/folder. ComfyUI skips model
   selection because workflows choose models. File selection does not establish
   compatibility.
4. **Save drafts** to finish later. Drafts retain application, address/path,
   model and launch choices; they remain separate from selectable workloads.
5. For Ollama, llama.cpp or vLLM, use **Supervisor-managed launch**. Review the
   instance name, launch port and model name/file/directory. Additional controlled
   options are under **Advanced launch options**. Applications and models must
   already exist; setup does not install or download them.
6. Select **Preview managed launch and add for review**. This derives a profile,
   unit identity and content hash; it does not write a unit or start a workload.
7. Alternatively, use **Advanced launch binding** to adopt an existing isolated
   service. Supply its service, cgroup and health URL, plus native instance/model
   identity and launch file where relevant. **Verify binding and add for review**
   fingerprints the existing file without changing it.
8. Select **Review configuration**. Switch to Idle and finish active jobs before
   confirming **Apply configuration**. Apply writes reviewed supervisor-owned
   units and commits the catalog through the existing activation safeguards.

llama.cpp and vLLM models use distinct units. Ollama models with the same instance
name and port can share a managed unit. Setup also preserves an adopted shared
pair unchanged from the accepted catalog. New or edited adopted shared bindings
require `gpu-mode configure`; setup does not verify them. The backend
`render-owned` command returns a preview only; Apply creates the managed launch
files. See [WORKLOADS.md](WORKLOADS.md). Model identity is checked again before
GPU admission.

Configured workloads can be renamed or edited. **Edit managed launch** reopens
the managed configuration for a new preview and review. **Remove from supervisor** changes
only supervisor configuration. Active/referenced workloads cannot be removed;
finish their work and switch to Idle first. Applications, models, workflows and
external configuration are preserved.

**Set up later** closes the window without applying configuration. Save drafts
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
inspects recognizable user-service launch metadata. A service candidate and an
endpoint candidate are separate observations; discovery does not correlate them
or verify lifecycle ownership. Existing catalog profiles remain unchanged.

For a non-default endpoint or explicit reference:

```sh
printf '%s' '{"app":"ollama","endpoint":"http://127.0.0.1:11434"}' | gpu-setup probe
printf '%s' '{"app":"llama.cpp","reference":"/models/model.gguf","referenceKind":"model-file"}' | gpu-setup probe
```

`app` accepts `comfyui`, `ollama`, `llama.cpp`, `vllm`. Select exactly one
`endpoint` or `reference`. `referenceKind` accepts `application`, `configuration`,
`model-file`, `model-directory`. Endpoint values are loopback HTTP(S) origins,
without credentials, path, query or fragment. References must be absolute clean
paths; only regular files and directories are accepted. File contents are not
executed or inspected, and directories are not enumerated.

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

All candidates have `lifecycleControl: "unverified"`. They cannot activate a
workload by themselves. Application management and model switching require the
separate validated catalog/runtime path.

Discovery uses fixed GET routes, disables redirects and ambient proxies, limits
responses to 1 MiB, and bounds each probe to 3 seconds and discovery to 15 seconds.
It never uses llama.cpp reload/routed autoload requests, starts applications,
loads/unloads models, downloads files, scans drives, or writes application or
supervisor configuration. Configuration files with custom syntax, shell wrappers
and escaped launch commands remain explicit manual references.

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
