# Local operator protocol

[Documentation](README.md) | [Repository](../README.md)

`/usr/bin/gpu-operator` is an on-demand, one-request process for the local
account. It accepts no arguments and never listens on a socket. Its authority is
that account's existing operator authority; it does not change automation grants.
Setup and runtime control are separate entrypoints.

The process resolves the effective UID through the OS account database, ignoring
`HOME` and XDG overrides. It reads
`~/.config/gpu-workload-supervisor/operator.json` through anchored, no-follow
file descriptors. The home and directory chain must not be group/world writable;
ancestors may be root-owned, and the regular profile must be account-owned.
Setup writes the profile; runtime requests cannot modify it.

The version 1 profile contains `statePath`, `activatedRelease`, `systemctlPath`,
`nvidiaSMIPath`, `gpuIndex`, optional `capacityHeadroomMiB`,
`statusTimeoutSeconds`, and `operationTimeoutSeconds`. Paths are absolute and
clean. The live catalog comes only from the accepted SQLite catalog, never an
edited configuration file. After acquiring the controller gate, the process
checks deployment activation before opening or migrating state.

## Request and response

Write exactly one compact JSON document followed by a newline, then close stdin.
Input is limited to 16 KiB and two seconds. Unknown, duplicate, case-aliased and
trailing fields/documents are rejected. The 1-64 byte request ID consists of
ASCII letters, digits, hyphen, underscore or period and is only a correlation
identifier, never an execution lease.

```json
{"protocolVersion":1,"requestId":"r1","action":"status"}
```

The response contains `protocolVersion`, `requestId`, and `code`. A successful
response to `status` and the transition actions additionally contains `status`
with:

- `owner`, `desiredWorkload`, `activeWorkload`, `phase`, `health`, `admission`;
- `observedAt` (UTC RFC3339);
- `expected`: `incarnation`, `version`, `owner`, `configurationRevision`;
- `workloads`: bounded `{id,label}` entries including Idle;
- `capabilities`: `takeControl`, `userSwitch`, `returnControl` booleans.

Versions are canonical nonzero unsigned 64-bit decimal **strings**. Do not parse
these through JavaScript Number. IDs use the catalog's lowercase identifier
rules. Labels use the catalog's bounded display validation. Unknown is never
selectable.

Mutations copy the complete `expected` object from a fresh successful status:

```json
{"protocolVersion":1,"requestId":"r2","action":"user-switch","target":"speech","expected":{"incarnation":"opaque","version":"42","owner":"user","configurationRevision":"opaque"}}
```

Only `user-switch` accepts a target. `take-control` transfers Supervisor ownership
to User while preserving the freshly verified current workload. It drains work
and verifies without lifecycle changes, including on failure. `return-control`
requires User ownership and stops work before committing Supervisor ownership in
Idle. The store checks incarnation, version, owner and configuration revision in
the transition transaction before effects. A repeated request is never replayed.

## Operator settings

`get-settings` takes no `expected` and answers from durable state without
observing the runtime. A successful response contains `settings` — and never
`status`, because an unobserved durable snapshot is not a fresh observation:

```json
{"protocolVersion":1,"requestId":"r3","action":"get-settings"}
```

```json
{"protocolVersion":1,"requestId":"r3","code":"ok","settings":{"policy":{"timeoutMinutes":0},"settingsRevision":"opaque"}}
```

`policy.timeoutMinutes` is 0 (Off) or 5–1440 minutes. `settingsRevision` is an
opaque concurrency token rotated on every committed write.

`set-idle-policy` carries the complete `expected` object from a fresh
successful status plus a `settings` object whose `settingsRevision` is copied
from a fresh successful **get-settings** (never from status, which does not
carry it):

```json
{"protocolVersion":1,"requestId":"r4","action":"set-idle-policy","expected":{"incarnation":"opaque","version":"42","owner":"supervisor","configurationRevision":"opaque"},"settings":{"timeoutMinutes":60,"settingsRevision":"opaque"}}
```

The store revalidates incarnation, version, owner, configuration revision and
settings revision inside the writer transaction; settings never interrupt an
unstable state or a running transition. A committed write rotates the settings
revision and clears any armed idle deadline in the same transaction. Enabling
(`timeoutMinutes` nonzero) is rejected with `unavailable` while the hosting
process has no qualified idle-evidence provider; a stale settings revision is
`stale_state`, a running transition is `busy`, and an out-of-bounds timeout is
`invalid_request`. The successful response contains the updated `settings`
object only. Both settings actions use the status-class lifetime. A
mixed-version peer that does not know these actions answers `invalid_request`
with the strict error shape, which clients treat as the idle policy being
unavailable.

## Demand activation

`activate-workload` starts a catalog workload from the idle, admission-closed
supervisor state. It carries the complete `expected` object from a fresh
successful status and a `target` workload (a configured profile, never
`idle`); no `settings` field. The store revalidates the operator precondition
plus owner=supervisor, phase=stable, health=healthy, admission=closed,
active/desired=idle, and zero unfinished admitted work inside the writer
transaction.

```json
{"protocolVersion":1,"requestId":"r5","action":"activate-workload","expected":{"incarnation":"opaque","version":"42","owner":"supervisor","configurationRevision":"opaque"},"target":"text"}
```

A committed activation rotates the lease fence; the successful response
additionally carries the fresh fence for exactly one execution generation:

```json
{"protocolVersion":1,"requestId":"r5","code":"ok","status":{"owner":"supervisor","...":"..."},"leaseFence":{"incarnation":"opaque","epoch":"7"}}
```

Activation never interrupts: a running transition stays `busy`, user
ownership is `wrong_owner`, a recovery latch is `recovery_required`, a stale
precondition is `stale_state`. When the precondition was readable but the
workload cannot start now — an active workload (`active != idle`) or
unfinished admitted work — the response is `deferred` and includes the
observed current `status` plus the current committed `leaseFence`, so a
caller that lost a committed activation response recovers the committed
generation on retry instead of being locked out by its own stale fence;
`deferred` latches nothing and may be retried.
Activation uses the mutation-class lifetime.

## Inactivity policy tick

`gpu-setup idle-policy-tick` is the oneshot policy evaluation driven by the
`gpu-workload-supervisor-idle.timer` user timer (every 60 seconds, gated on
`ConditionPathExists=%h/.config/gpu-workload-supervisor/operator.json`;
guided setup enables the packaged timer link alongside the reconciliation
unit, and remove-integration removes both). It reads the persisted
`operator.json` profile — failing loudly when the file is unreadable rather
than falling back to default paths — and opens the profile's configured state
path and runtime. (`gpu-mode -state <path> idle-policy-tick` remains the
explicit-path form for diagnostics.) The tick
reads the committed settings and durable state; an Off policy, user
ownership, a non-stable or latched state, or an active/unknown workload
no-ops after disarming any armed deadline. With no qualified evidence
provider configured, an enabled policy fails closed: the tick exits nonzero
and never idles. When qualified fresh evidence (attestation no older than 120
seconds) shows no queued/reserved/running/unresolved work and no unfinished
admissions, the tick commits a verified armed deadline (last activity +
timeout). Once the armed deadline has elapsed without newer activity, the
tick drains the workload into idle with the provider's evidence-generation
revalidation invoked inside the store's writer transaction, atomically with
the pending-work recheck under the single-writer lock; a revoked generation
aborts the commit, disarms, and fails closed. The residual window is only
provider-internal: evidence moving after the provider's own revalidation
returns is outside the store's reach, so providers must refuse the next
admission for work materializing in that gap. Any preemption — new activity, an
admission, a state change — aborts cleanly with exit 0 and no latch; the next
tick re-verifies from fresh evidence.

Status and mutations both use the same nonblocking controller gate. `busy` means
no request was queued. Errors contain no status and use exactly:
`invalid_request`, `unsupported_version`, `incompatible_configuration`,
`stale_state`, `wrong_owner`, `busy`, `recovery_required`, `timeout`,
`unavailable`, `deferred`. A `deferred` activation response additionally
carries the observed current status. Invalid request envelopes may have an
empty correlation ID. Error responses do not disclose profile paths or
runtime output.

## Lifetimes and limits

Status defaults to 30 seconds, with a profile maximum of 60 seconds. Mutation
execution defaults to 15 minutes, with a maximum of 30 minutes. Existing failure
cleanup can add two minutes and durable finalization can add ten seconds.
Response writing has a separate two-second limit and a 64 KiB cap. Clients should
allow 75 seconds for status and the settings actions and 33 minutes for
mutations, and age observations from monotonic dispatch time.

Before admission the process detaches its session and ignores SIGHUP and SIGPIPE;
failure to detach returns `unavailable`. The operation context is independent of
stdin and stdout. Execution and durable finalization finish before response
writing. Closing the output reader or disabling the client cannot cancel an
admitted transition. Broken output cannot trigger retries.

A lost response is an uncertain outcome: obtain fresh status before another
operator decision. SIGKILL, host shutdown, and session-manager logout policies
can still interrupt execution and require existing explicit recovery. Automated
subprocess tests exercise session detachment, SIGHUP and closed output pipes;
they do not qualify real GNOME Shell restart or logout behavior.

## GNOME Shell extension client

`clients/gnome/gpu-workload-supervisor@local` implements the GNOME Shell 50
extension shipped with the local operator. It uses native SystemIndicator and
QuickMenuToggle controls and invokes only `/usr/bin/gpu-operator`, with one bounded
JSON request per process. No proxy, gateway, Job Broker, listener or persistent UI
service is required. The local OS account supplies operator authority; ownership
confirmation expresses intent, not verified human presence.

The committed toggle is OFF under Supervisor ownership and ON under User ownership.
Take Control confirms preserving the verified current workload. Stop and Return
confirms stopping work and returning Supervisor ownership in Idle. User workload
selection is immediate; selecting the active workload does nothing. Normal menus
show configured labels. Details retains owner, requested and active workloads,
phase, health, admission and observation freshness bounded to thirty seconds
after dispatch.

- The client never optimistically changes committed ownership.
- Pending calls, stale observations, errors and recovery requirements disable
  mutations.
- One call may be outstanding; status polling backs off from five to sixty
  seconds and never overlaps a mutation.
- Freshness expires thirty seconds after dispatch, using monotonic time.
- Versions remain decimal strings. Retired enable generations, stale decisions
  and lower same-incarnation versions are rejected.
- Disconnects require fresh status; mutations are never replayed.
- Client read deadlines exceed the backend's maximum operation plus
  cleanup/finalization and input/output budgets; the numbers are in
  [Lifetimes and limits](#lifetimes-and-limits).
- A client deadline leaves the outcome uncertain.
- Disable cancels local reads and removes UI resources, without killing the
  backend.
- The backend must independently preserve admitted operations across Shell
  restart.
- There is no recovery control in the extension.

Run contract, model, and mocked lifecycle tests with `npm test --prefix clients/gnome`.
Run the native bounded-stream smoke test with
`gjs -m clients/gnome/tests/transport.gjs.js`. The latter needs GJS and is not a
Shell qualification. Before enablement, qualify a real GNOME 50 session: keyboard
and visual access to the toggle/menu/Details, cancel and confirm both ownership
dialogs, third-workload selection, active no-op, pending repeated clicks, stale and
faulted observations, disable/re-enable and Shell restart during admitted work.
Confirm backend survival and later fresh status with the actual deployment.
Headless tests do not provide this evidence. Packaging alone does not enable the
extension or qualify a distribution.

Browser automation is not implemented. There is no dashboard or browser client,
and no transport grants browser callers the local operator's authority.
