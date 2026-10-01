# Dashboard and local desktop client plan

[Documentation](README.md) | [Repository](../README.md)

Plan for [issue #61](https://github.com/mickey-kras/gpu-workload-supervisor/issues/61).
Future clients use the [restricted external control contract](external-control.md)
implemented by #60. Neither client is implemented or part of initial deployment.
Implementation and enablement require the gates below.

## Repository and shared package

Implement both clients in one dedicated client repository, separate from this Go
supervisor repository. Its future workspace will contain:

| Placement | Responsibility |
| --- | --- |
| `packages/control-client` | Shared TypeScript v1 request/response types, strict validation, lossless state-token handling, transport interface, and observation/request ordering rules. |
| `apps/dashboard` | Browser presentation and user interaction using that package through an approved authenticated adapter. |
| `apps/desktop-extension` | Local desktop extension presentation using the same package and an approved transport; no privileged host control. |

Create that repository and packages only in a separately authorized implementation
task. Keep this decision and the authoritative control/proxy contracts here.
Release the shared package and clients independently, pin their versions, and
record the supported supervisor API version. TypeScript permits both clients to
share the same validation and uncertainty handling; separate placement avoids
coupling frontend/extension tooling to supervisor lifecycle or release artifacts.
The package consumes the JSON contract, not Go `internal` packages. The server
contract remains language-neutral; conformance fixtures must detect client drift.

Inject a transport interface into the shared package. Its only v1 calls are
`status` and `request-workload` with allowlisted typed targets. The package must
not invent HTTP routes, resolve identities from request bodies, spawn commands,
invoke systemd, read the state database, or fall back to direct runtime control.
Transport authentication, principal resolution, grants, audit sinks, and exposure
belong to a separately reviewed server adapter/deployment boundary. UI controls
may restrict choices but never grant authority; v1 has no capability-discovery
endpoint, and the server must enforce each operation and target grant.

The supervisor remains the sole lifecycle engine. The broker owns workflow
queues, job state, prompts, and outputs. Selecting a workload neither reserves
capacity nor proves a job ran or completed. Execution and terminal completion
retain the [proxy contract](PROXY-CONTRACT.md), including its admitted correlation
tuple and verified terminal evidence; clients cannot infer completion from a
switch response, enqueue acceptance, stream end, or disconnect.

## Authoritative display and freshness

Validate the v1 response before using it. Only `code: ok` includes `status`; all
errors omit it. Display the six authoritative fields without deriving ownership
or admission from a selected button or pending request:

| Field | Display meaning |
| --- | --- |
| `owner` | Committed `supervisor` or `user` ownership. |
| `desiredWorkload` | Committed target, distinct from a local requested target. |
| `activeWorkload` | Observed `text`, `media`, `idle`, or `unknown`; never infer it from desired state. |
| `phase` | Reported transition phase; a pending client call is a separate indicator. |
| `health` | Reported health; `healthy` alone is insufficient for execution readiness. |
| `admission` | Supervisor admission, separate from user execution and runtime UI availability. |

Also retain `expected.incarnation`, `expected.version`, `updatedAt`, and
`observedAt`. These are the complete status DTO: it supplies no lease epoch,
transition journal, arbitrary runtime error, job state, or workload data.
`updatedAt` dates durable state; `observedAt` dates the backend return. Neither
requires the client and server wall clocks to agree.

Track freshness with a configured finite budget, local request ordering, and
monotonic elapsed time since the accepted observation's request was dispatched
(a conservative age bound that includes response delay). Receipt alone must not
make a delayed response fresh; retain its receipt time for connection tracking.
If dispatch age exceeds the budget on arrival, mark the result stale and keep
mutations disabled, even when its token matches the current state.
Show last-known values with a conspicuous stale/unavailable label after that
budget, failed refresh, authentication loss, disconnect, or malformed/unsupported
response. Disable mutations until a new valid authorized observation is accepted.
Never replace missing status with idle defaults. Healthy supervisor idle requires
a fresh observation with supervisor ownership, desired and active `idle`, stable
phase, healthy health, and closed admission. Label user-owned idle separately.

Status takes the same exclusive gate as mutations and CLI control. It can return
`busy`, and observation failures can latch controller faults. It is not a
lock-free readiness probe or a side-effect-free heartbeat. Use one outstanding
refresh at a time, bounded polling with backoff and a configured maximum rate;
do not spin on `busy`, poll over an in-flight mutation, or use polling to recover.
A gate held by another process may prevent fresh transition-phase observations;
show that uncertainty instead of inventing progress.

## Conditional operations and response ordering

1. Before deciding to request a workload, obtain fresh authorized status. Confirm
   the state is supervisor-owned, stable, non-error, and the target is allowed by
   the approved client policy; server checks remain authoritative. Send exactly
   the returned `expected.incarnation` and `expected.version`. This token is not
   the execution lease incarnation/epoch and is not a reservation.
2. Preserve the positive uint64 version losslessly in parsing and serialization;
   never round through a JavaScript `Number`. Incarnation and version are one
   token. Reject unrepresentable/invalid tokens rather than manufacture values.
   Serialize only the exact v1 envelope, within its 4096-byte bound, with no
   identity, command, recovery, workflow, or extra fields.
3. Allow one local workload request in flight. Disable repeat clicks and other
   mutation controls before dispatch. Keep the requested target separate from
   observed state; do not optimistically update ownership, health, or admission.
   This is local click suppression, not server deduplication or idempotency.
4. Use a local connection/authentication generation and request sequence. Discard
   responses from retired generations or requests superseded by a mutation or
   newer accepted observation. Within an incarnation, never replace an accepted
   token with a lower version. Equal versions must not refresh freshness using
   an older request. Do not numerically order unrelated incarnations or trust
   timestamp order. An accepted replacement incarnation invalidates all old
   tokens and pending decisions; require a fresh decision, without resubmission.
5. Treat the synchronous response as the result of that call. An `ok` mutation
   status remains an observation, not ongoing readiness. On uncertain outcomes,
   keep mutations disabled and refresh; if refresh is busy/unavailable, remain
   uncertain until a fresh result arrives. Do not start another mutation merely
   because the client canceled or disconnected.

No workload mutation is automatically retried, queued, or replayed, including on
reconnect. There is no v1 idempotency key. Cancellation can include bounded
controller cleanup/finalization time and can leave admission closed or a
transition requiring recovery. A result-audit failure returns `unavailable`
after effects may have committed, and can override `timeout` or `canceled`.
None of these results proves rollback or absence of effects.

| Result | Client behavior |
| --- | --- |
| `unauthenticated`, `forbidden` | Show denial, invalidate usable observations, and require valid authorized access before refresh/decision; no alternate endpoint or identity fallback. |
| `invalid_request`, `unsupported_version` | Show compatibility/validation failure and disable affected controls; do not loosen the envelope or guess a version. |
| `user_owned` | Refresh and display ownership; automated selection cannot override user control. |
| `stale_state` | Refresh both token components and require a new decision; do not update the token and replay the old intent. |
| `busy` | Show gate/transition contention, retain stale last-known values, and back off status refresh; never retry the mutation automatically. |
| `recovery_required` | Show the recovery requirement and keep mutations blocked; operator investigation is required. |
| `timeout`, `canceled`, `unavailable`, transport failure | Show an uncertain outcome, refresh before a new decision, and never assert that nothing changed. |

## Human authority and runtime UI boundaries

Ownership transfer, stopping current user work, user switching/return, and recovery
require separate explicit human authority. These operations do not exist in v1;
the planned UI must mark them unavailable, with no command, admin flag, shell,
systemd, CLI bridge, or UI endpoint that extends an automated principal's grant.
A confirmation dialog alone cannot supply missing server authority.

Enabling such controls requires a separately approved human contract that binds
verified human authorization to the specific target and effects, explains which
current work may be stopped, records the decision/outcome, and fails closed when
authority is absent. Its acceptance must cover latched failures and recovery
without restarting stopped user work. This plan does not design or grant that
contract. Until it exists and is qualified, both clients leave these actions
unavailable and direct the human to the separately authorized operator procedure.

Keep runtime UI routes and supervisor control routes distinct. Link to a runtime
UI only through deployment-approved routing with its own authentication and
origin rules; its availability depends on the runtime/stop policy. A link is not
readiness or execution permission. Do not embed a runtime UI as an admission
bypass, share supervisor credentials with it, or broaden proxy classifications
to make it accessible. Runtime execution still needs the qualified proxy/broker
boundary; exact route classifications and alternate channels require evidence.

For browser/session adapters, preserve exact Origin allowlists, CSRF protection
for mutations, restrictive CORS, and no wildcard credentialed origin. Origin
checks supplement authentication. Desktop locality supplies no identity or
privilege; its host bridge must expose only the approved typed transport, never
general command execution. Keep credentials out of URLs, runtime UI contexts,
logs, and client error text; revocation must disable authenticated operations.

## Acceptance tests before enablement

Run these against both clients and the shared package, with the reviewed adapter
and deployment boundary. Fixtures must cover the exact v1 DTO/enums/result codes,
invalid responses, lossless uint64 tokens, and the absence of status on errors.

| Scenario / issue acceptance | Required evidence before enablement |
| --- | --- |
| Authoritative state and healthy idle | All six fields match server observations; pending selection does not change them; unknown active state and user-owned idle remain distinct from healthy supervisor idle. |
| Stale status / unavailable status | Expired monotonic age, long-delayed responses even with a current token, failed refresh, and skewed wall clocks label last-known state and disable decisions; missing status never renders idle. |
| Typed operations and capability boundary | Only exact v1 operations/targets are emitted; hostile extra fields and UI tampering cannot add authority; no shell, systemd, database, or direct-runtime fallback exists. |
| Duplicate clicks / in-flight transition | Repeated clicks yield one dispatch; pending calls disable mutations; external gate contention returns busy without invented progress or automatic mutation retry. |
| Disconnection / cancellation / audit failure | Lost responses and unavailable-after-effects remain uncertain; reconnect refreshes without replay; cleanup delay does not re-enable controls prematurely. |
| Rejected authorization | Missing, invalid, expired, revoked credentials and target denial fail closed; cached state cannot enable mutations or leak across authenticated contexts. |
| Latched recovery / explicit human authority | Recovery-required and faulted status remain blocked; absent human contract leaves transfer/stop/recovery unavailable; any future human controls require independently verified target/effect-specific authority. |
| Incarnation replacement | A replacement/restored store at the same numeric version invalidates old tokens/intents; requests cannot reuse old incarnation or lease epoch as the version. |
| Out-of-order status and mutation responses | Late polls, older versions, retired connection/auth contexts, and old-incarnation responses cannot overwrite newer accepted state or freshness. |
| Runtime UI authentication/origin isolation | UI links/routes cannot reuse supervisor authority or bypass admission; cross-origin, CSRF, credential leakage, redirects/upgrades, desktop bridge, and direct runtime access are tested at the deployment boundary. |
| Broker/execution separation | Selection/acceptance/EOF do not render jobs complete; the broker/proxy retains verified admission and terminal-completion correlation. |

## Enablement gates

Before either client is enabled, record evidence for all of the following in the
implementation/deployment review:

- #60 is implemented and verified on main, and a pinned backend release containing
  it has passed backend qualification. `v0.1.5` predates #60; the planned version
  in `release-version.json` is not itself a published/qualified release.
- The deployment has passed [host qualification](DEPLOYMENT.md#qualify-the-host)
  and [proxy qualification](PROXY-CONTRACT.md), with direct access
  and independent runtime activation unable to bypass the intended boundary.
- The authenticated transport adapter and client security boundaries are approved
  and qualified, including authorization, revocation, audit, bounded reads,
  origin/CSRF/CORS, and runtime UI isolation.
- The shared package and both clients pass the future matrix above with pinned
  versions, reviewed freshness/polling limits, and no automatic mutation replay.
- Human-only controls remain unavailable. Enabling any of them additionally
  requires the separately approved human authority contract and its acceptance
  evidence; an automated grant cannot satisfy this gate.

Repository CI and this planning document provide no host-specific acceptance.
Concrete deployment configuration and evidence remain outside this repository.
