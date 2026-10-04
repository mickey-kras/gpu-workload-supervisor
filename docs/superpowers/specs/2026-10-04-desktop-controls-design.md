# Local GPU desktop controls design

Status: proposed architecture for user review; no implementation or host qualification.
Base reviewed: b4f60c6cd06fa9c4f1e6285e4f70adc3b308d37f.
Scope: #147, #145, #146, #148. Written issue acceptance criteria remain authoritative.

## Outcome and sequence

Install the control plane once, configure existing workloads through guided setup,
then control one GPU from GNOME Shell 50 without manually starting a UI service.
Workload applications and models remain user-provided.

Implement #147 workload profiles first, then #145 operator integration. GNOME
presentation may develop against agreed contract fixtures in parallel. Finish
#146 integration and #148 end-to-end packaging after backend acceptance.
No demand activation, auto-idle, savings claims, remote dashboard or UI recovery.

## Workload configuration (#147)

Use strict standard-library JSON for a versioned backend-owned catalog. Each
profile has a stable ID, display label, supported lifecycle/health/release adapter,
validated unit/cgroup and endpoint configuration, capacity requirements and explicit
boot policy. IDs are bounded lowercase ASCII; idle and unknown are reserved.
Idle means no managed workload; unknown cannot be selected.

Retain Text and Media as compatible defaults by translating existing CLI flags.
Reject conflicting legacy flags and file configuration. Add generic
switch --workload ID while preserving existing text/media/idle commands.

Generalize runtime observations and lifecycle checks by workload ID. Before any
target starts, verify all opposing configured workloads have released the GPU.
Preserve current Text boot retention and Media stop-to-idle behavior through explicit
policies. New profiles default to stop-to-idle. HTTP unload success alone never
proves release; preserve the existing fail-closed live Media unload restriction.

Persist the accepted catalog, opaque revision and state-version advancement in
one SQLite transaction under the controller gate. Pin that immutable catalog
snapshot for the entire admitted operation. The private deployment profile selects
trusted absolute state/install paths and limits; it is not an alternate live catalog.
Changing the state location is an explicit maintenance migration, not a runtime edit.
Setup validates and commits configuration through this transaction;
ordinary status/mutation calls do not silently adopt changed files. Reject stale
revision requests. Reject removal or incompatible changes to active, desired,
unfinished-work or unfinished-transition references. Preserve historical records
and profile information needed by recovery. Failed updates leave the old catalog
effective. New configuration never expands an automation grant.

Rebuild SQLite tables whose constraints hardcode text/media through a transactional
migration, preserving ownership, incarnations, epochs, versions, work tokens,
journals, indexes and foreign keys. Existing binaries must reject the new schema.
Rollback uses the established compatible-backup restore protocol, never a metadata
edit or an automatic database downgrade.

Primary code areas: internal/control, runtime, supervisor, store, externalcontrol,
proxy; both current command entrypoints. Tests cover populated migrations,
third-workload lifecycle/recovery/admission/completion, opposing-runtime release,
invalid/overlapping profiles, referenced-profile edits, config races, failed reload,
and legacy-grant denial for a newly configured workload.

## Local operator contract (#145)

Add an on-demand gpu-operator entrypoint. Native package invocation is
/usr/bin/gpu-operator with fixed arguments; requests contain no executable,
configuration, unit or cgroup paths. Resolve the effective account's home through
the OS account database and read
.config/gpu-workload-supervisor/operator.json beneath it. Do not accept a HOME,
XDG or request override for runtime profile resolution. Validate the regular file,
owner and directory chain; reject symlinks and group/world-writable paths. Setup
uses this same convention and never needs root to write the private profile.
Setup and runtime control
are separate command paths; runtime cannot write configuration.

One bounded newline-delimited JSON request per process:
protocolVersion, requestId, action; mutations include expected with incarnation,
decimal-string version, owner and configurationRevision. Actions are status,
take-control, user-switch and return-control. Only user-switch accepts target.
Reject unknown fields, trailing JSON, invalid tokens and unsupported versions.
The request token is separate from execution leases.

Take-control preserves the freshly verified current workload. Return-control
always stops current work and transfers Supervisor ownership in Idle.
Add a distinct conditional operator transition path, checking incarnation,
version, owner and configuration revision atomically before effects. Preserve
the Supervisor-only automation transition and explicit workload grants.

Use existing nonblocking TryAcquire before opening state. Status uses the same
gate because observations can latch faults. Return busy without queueing/replay.
Status advertises bounded workload labels/IDs, capabilities, revision, committed
owner/desired/active/phase/health/admission, observation time and state token.
Typed errors carry no usable status: invalid_request, unsupported_version,
incompatible_configuration, stale_state, wrong_owner, busy, recovery_required,
timeout and unavailable. These underscore-separated codes are the shared fixture
vocabulary; profile trust
failures map to incompatible_configuration and unexpected runtime/output failures
to unavailable.

Proposed resource limits: 16 KiB request, 64 KiB response, 64 ASCII bytes request ID,
2-second input/output budgets; status default 30 seconds with 60-second maximum;
operation default 15 minutes with 30-minute maximum in trusted configuration.
Existing bounded cleanup/finalization is additional and must be documented.

After admission, transition execution must not depend on UI stdin, output-reader
lifetime or cancellation. Finish and persist before writing the final response;
broken pipes cannot cancel effects. Detach the backend session and handle SIGHUP;
never bind its operation context to
UI read cancellation. Process survival across actual Shell restart is an acceptance
gate, including process-group behavior. Host shutdown, SIGKILL and logout policy
still require the existing interrupted-transition recovery. A disconnected client
marks outcome uncertain and refreshes; no retry or replay.

Authority is the existing local OS account, equivalent to operator CLI access.
Confirmation conveys intent, not verified human presence. No listener, shell,
arbitrary command dispatch or new remote/automation authority.

Tests include stale/restored tokens, wrong owner, configuration races, contention,
duplicate requests, malformed input, profile trust, failing health, latches,
deadlines, output bounds and client/Shell disappearance.

## GNOME Shell 50 client (#146)

Place native GJS extension in clients/gnome, with separate pure contract/model
modules, Gio transport, ownership dialogs and Shell rendering. Use native
SystemIndicator and QuickMenuToggle. Disable automatic toggle state changes:
only accepted backend ownership sets the committed toggle.

- OFF: Supervisor owns GPU; current workload is read-only.
- Turning ON confirms takeover and preservation of current verified workload.
- Turning OFF confirms stopping work and returning Supervisor ownership in Idle.
- User workload/Idle selection is immediate without confirmation; active mode is a no-op.
- Pending, stale/unavailable, incompatible and recovery-required states disable mutations.
- Details shows owner, requested/active workload, phase, health, admission and freshness.
- Normal menus use configured labels; raw IDs, paths and logs remain outside them.

Use bounded asynchronous Gio stream reads with fixed argv, never unbounded output
collection or shell commands. Maintain one outstanding call, no polling during
mutation, and bounded status backoff. Age observations from monotonic dispatch
time, not receipt. Reject retired enable generations, superseded sequences and
lower same-incarnation versions. Incarnation replacement invalidates old decisions.
Decimal-string versions never pass through JavaScript Number.

Disable removes signals, timers, dialogs and widgets, retires callbacks and never
kills an admitted backend transition. Uncertain outcomes require fresh status.
No optimistic success, automatic mutation retries, direct systemd/runtime/database
access or recovery button.

Use Node's built-in test runner for pure fixtures and native Gio tests for transport.
Cover the issue's complete acceptance matrix and a third workload. Run real GNOME
50 tests, then deployment-host visual/keyboard/transition/lifecycle qualification
before enabling. Headless tests do not qualify Shell behavior.

Rewrite docs/CLIENTS.md to separate local operator controls from deferred restricted
browser automation. Supersede its separate-client-repository and hypothetical
human-presence requirements for this native client. Do not add proxy, gateway or
Job Broker dependencies to local controls.

## Single installation and setup (#148)

Recommend the existing GoReleaser pipeline with its established nFPM integration
to deliver one .deb containing a compatible backend/extension pair, GJS
Gtk4/Libadwaita setup app, desktop launcher and user reconciliation unit template.
Initial Shell target remains 50. Declare exact Debian-family distribution and
architecture support only after qualification; existing archive architectures do
not prove desktop package support.

Alternatives: an extension-store-only bundle cannot include the Go binary under
current GNOME review rules; a bespoke archive installer would duplicate package
ownership/upgrade machinery. Retain existing archives for current CLI users.

Setup runs in the operator account. It discovers supported candidates, lets the
user select/configure them, submits validation to the backend and previews concrete
unit/configuration changes. Never auto-adopt discovered services. Install only
owned integration/drop-ins and reconciliation, with an ownership manifest.

Setup stages and validates files, durably records planned/previous owned files,
then applies prerequisites while retaining the old effective catalog. Before
changing any runtime-affecting prerequisite, acquire the controller gate and verify
a safe maintenance state with no admitted work or transition. Reject changes to
live referenced profiles. Commit catalog/revision/state version together only
after prerequisites succeed, then finalize the manifest. Pre-commit failure
restores only setup-owned changes; post-commit failure keeps the committed catalog
and resumes finalization. Neither path restarts workloads or silently rolls back
live mappings. Tests inject failure at every boundary.

Never silently stop workloads, overwrite user units or change current ownership.
Package maintainer scripts do not enumerate user sessions, open state, start
workloads or configure arbitrary users.

No permanent daemon solely for the UI. Setup handles reconciliation configuration
without manual service commands and clearly handles unsupported Shell/session
requirements. Verify whether logout/login is necessary for extension discovery.

The setup application owns upgrade activation. Package replacement alone leaves
the new operator entrypoint incompatible with the deployment's activated release
marker; it must not open or auto-migrate that state. The same deployment activation
check applies to CLI, proxy and reconciliation entrypoints. Existing legacy
deployments enter this managed scheme only through explicit setup adoption.

For activation, setup obtains the controller and proxy maintenance gates, prevents
new admission and verifies that existing work and runtimes have been safely
quiesced under an explicit operator decision. It records a durable maintenance
marker, verifies a consistent backup, performs the migration and validation, then
updates the activated release marker and clears maintenance. Every managed state
opener checks maintenance before effects. A crash leaves maintenance in force;
resume through setup or explicit CLI recovery, never automatic workload restart.
The migration/activation protocol must test old processes holding gates, newly
started commands, reconciliation and crashes at each boundary. Preserve the prior
verified binary/configuration with the backup for supported rollback. Rollback follows RESTORING.md:
compatible exact binary/config/state pair or controlled backup restoration with
fence rotation; otherwise repair forward. Interrupted setup resumes from its
manifest without resetting state. Uninstall preserves user workload units, models,
profiles, state and audit; remove only package/setup-owned integration.

Acceptance: clean install, invalid/repeated/interrupted setup, existing unit
preservation, Shell version mismatch, live-operation survival, upgrade, rejected
unsafe downgrade, supported restore, uninstall/reinstall and artifact verification.

## Review and completion

Each implementation PR receives independent review and testing. GNOME acceptance
includes the standard ten lenses; add migration, GPU safety and packaging reviews
where relevant. Resolve blocker/major/minor findings and retain all existing
quality/security/release gates. Squash merges only, by the orchestrator.

Repository tests and host acceptance are recorded separately. Do not close an
issue whose required host acceptance remains unverified. Host-specific profiles
and automation stay in private homelab.

## Sources

- [GNOME review rules](https://gjs.guide/extensions/review-guidelines/review-guidelines.html#scripts-and-binaries)
- [Quick Settings](https://gjs.guide/extensions/topics/quick-settings.html)
- [GNOME 50](https://gjs.guide/extensions/upgrading/gnome-shell-50.html)
- [Gio subprocesses](https://gjs.guide/guides/gio/subprocesses.html)
- [GoReleaser nFPM](https://goreleaser.com/customization/package/nfpm/)
- [Restore and rollback](../../RESTORING.md)
