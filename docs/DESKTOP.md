# Local GNOME desktop installation

[Documentation](README.md) | [Clients](CLIENTS.md) | [Restore procedure](RESTORING.md)

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
loopback health URL, measured VRAM requirement, and login policy. Applications,
models, and workload service units must already exist. New workloads default to
stop-to-idle at reconciliation; retain is an explicit choice. Live media unload
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
Ordinary controls never adopt edits to that mirror. Setup's `discover`, `validate`,
and `apply` commands use a strict versioned JSON request over stdin; the desktop
application supplies this request and renders the concrete preview.

## Upgrade and interrupted activation

Before replacing the package, use the currently activated controls to finish or
explicitly stop admitted work and bring the deployment to stable, healthy Idle
with admission closed. Stop proxies and prevent external workload automation from
restarting during maintenance. This is an explicit operator decision; package
installation and setup do not perform it for you.

After package replacement, the new entrypoints reject the previous activated
release before opening or migrating state. Open setup to activate the new release.
Setup obtains both controller and exclusive proxy gates, verifies the existing
and proposed committed mappings have released the GPU, checks state read-only,
and makes an integrity-checked SQLite `VACUUM INTO` snapshot including WAL content.
Each activation retains its own snapshot under the private `backups/activation-*`
directory. Managed snapshots retain the previous binary checksums, profile,
accepted catalog, deployment marker, and state together.

A durable maintenance marker is written before migration. The owned-file journal
records before/after images before applying prerequisites. Pre-commit failure
restores only owned integration; a known committed catalog is never rolled back.
Post-commit failure retains maintenance and resumes forward. Reopening setup
recovers the exact pending request, including whether the database was newly
created. A stale preview is rejected before entering maintenance. Do not delete
maintenance or migration metadata to bypass a failure.

## Restore and rollback

There is no automatic downgrade. Follow every step in [RESTORING.md](RESTORING.md),
including stopping and preventing all state users, handling WAL/SHM correctly,
and running `restore-state` to rotate the fence before reconciliation.

For a managed rollback, select one matching verified backup directory and restore
its complete state, profile, ownership metadata, and compatible binary set together. Install the matching backend/extension package as well. The saved
`manifest.json` records the release and SHA-256 of each saved executable. Saved
binary files deliberately have private mode `0600`; verify the hashes before
making a chosen recovery copy executable with mode `0700`. Never substitute an
unrelated installed binary or edit release/schema metadata to force compatibility.
Restore the matching setup metadata to these locations:

| Backup file | Private destination beneath `~/.config/gpu-workload-supervisor/` |
| --- | --- |
| `operator.json`, `catalog.json`, `ownership.json` | The corresponding files directly in this directory |
| `manifest.json` | `activated-binaries/manifest.json` |
| `gpu-mode`, `gpu-workload-proxy`, `gpu-operator`, `gpu-setup` | Corresponding files in `activated-binaries/` |

Restore `state.db` to the profile's original state path and
`state.db.deployment.json` beside that database with the same basename. While all
components remain stopped, remove the failed activation's `transaction.json` and
`activation.json` from the private configuration directory: their plans belong to
the failed tuple, not to the restored one. Preserve them with the failed deployment
for investigation. Retain the restored ownership hashes so subsequent setup can
recognize its files. The profile's state path must remain the original deployment path. Preserve the
failed deployment separately, then run the matching release's `restore-state`
and recovery procedure. If the exact pair cannot be verified, repair forward.

## Remove and reinstall

Before removing the package, `gpu-setup remove-integration` removes only the
recorded reconciliation enablement link, without stopping a service. Modified or
unowned links are preserved. Disable the extension through GNOME Extensions.
Package removal removes package-owned binaries, extension, launcher, and unit;
profiles, workload units, models, state, audit, and backups remain private user
data. Reinstall the matching release or activate a newer one through setup.
