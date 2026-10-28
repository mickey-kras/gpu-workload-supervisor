# Local operator protocol

[Documentation](README.md) | [Repository](../README.md)

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

## Request and response

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

## Operator settings

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

## Demand activation

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

## Inactivity policy tick

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

## Lifetimes and limits

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

## Client for the GNOME Shell extension

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
