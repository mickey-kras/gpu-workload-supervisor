# External control contract

`internal/externalcontrol` provides a transport-neutral service for authenticated
automation to inspect state and request an approved supervisor workload. It does
not provide a listener, CLI command, browser UI, or deployment configuration.
An adapter must establish its own authenticated transport before calling the
service. This contract grants no ownership-transfer or recovery authority.

## Versioned request envelope

The service accepts one JSON object, at most 4096 bytes including whitespace.
Field names are exact and case-sensitive. Unknown fields, duplicate keys,
multiple JSON values, trailing non-whitespace data, incompatible fields, invalid
types, and null or missing required values are rejected. There is no permissive
extension bag: clients must use the supported `apiVersion`, currently `v1`.
An unsupported nonempty version returns `unsupported_version` after the outer
object is checked, before operation-specific validation.

`Handle` receives an already allocated byte slice. Its size check bounds accepted
payloads, not network-read allocation. Future adapters must impose body-read and
transport limits before allocating or buffering that slice.

| Field | `status` | `request-workload` |
| --- | --- | --- |
| `apiVersion` | Required: `"v1"` | Required: `"v1"` |
| `operation` | Required: `"status"` | Required: `"request-workload"` |
| `target` | Forbidden | Required: `"text"`, `"media"`, or `"idle"`; also subject to the caller's grant |
| `expected` | Forbidden | Required object containing `incarnation` and `version` |
| `expected.incarnation` | — | Required 1–128 byte string using ASCII letters, digits, `-`, or `_`, from the returned state token |
| `expected.version` | — | Required positive unsigned 64-bit integer from the returned state token |

```json
{"apiVersion":"v1","operation":"status"}
```

After obtaining a state token, send both components unchanged when requesting a
workload. This example illustrates the envelope; obtain the actual token through
authorized status rather than inventing one:

```json
{
  "apiVersion": "v1",
  "operation": "request-workload",
  "target": "media",
  "expected": {"incarnation": "OBSERVED_INCARNATION", "version": 2}
}
```

Requests cannot supply identities, audit identities, timeouts, commands, paths,
service units, free-form reasons, ownership changes, recovery operations, prompts,
outputs, or workflow state. Future contract changes require an explicit supported
version; unknown fields are not a negotiation mechanism.

## Responses and result codes

Every response contains `apiVersion: "v1"` and a fixed `code`. An `ok` response
includes `status`; errors omit it, including authorization, audit, and timeout
failures. Arbitrary backend error text is never serialized. Transport status codes
and wire framing are adapter responsibilities.

```json
{
  "apiVersion": "v1",
  "code": "ok",
  "status": {
    "owner": "supervisor",
    "desiredWorkload": "text",
    "activeWorkload": "text",
    "phase": "stable",
    "health": "healthy",
    "admission": "open",
    "expected": {"incarnation": "OBSERVED_INCARNATION", "version": 2},
    "updatedAt": "2026-10-01T00:00:00Z",
    "observedAt": "2026-10-01T00:00:01Z"
  }
}
```

`updatedAt` is the durable state timestamp; `observedAt` records when the backend
call returned. Timestamps use JSON's RFC 3339 time representation. The DTO omits
lease epoch, transition journals, runtime errors, and workload data. Its enum
values come from the control state:

| Field | Values |
| --- | --- |
| `owner` | `supervisor`, `user` |
| `desiredWorkload` | `text`, `media`, `idle` |
| `activeWorkload` | `text`, `media`, `idle`, `unknown` |
| `phase` | `stable`, `draining`, `unloading`, `loading`, `verifying`, `reconciling` |
| `health` | `healthy`, `degraded`, `error` |
| `admission` | `open`, `closed` |

| Code | Meaning |
| --- | --- |
| `ok` | The authorized backend call and its audit succeeded; inspect the returned status. |
| `unauthenticated` | The resolver could not supply a verified nonempty principal. |
| `forbidden` | The principal has no matching grant or lacks permission for the operation/target. |
| `invalid_request` | The envelope, field combination, type, bound, or value is invalid. |
| `unsupported_version` | The requested API version is unsupported. |
| `user_owned` | A workload request cannot operate on user-owned state. |
| `stale_state` | The expected incarnation or version does not match. |
| `busy` | The process gate is held or a transition is already running. |
| `recovery_required` | The controller requires reconciliation or explicit recovery. |
| `timeout` | A request deadline, controller drain timeout, or verification timeout expired. |
| `canceled` | The request context was canceled. |
| `unavailable` | Audit, gate, backend, or response validation failed without a more specific code. |

For example, a denied request returns only:

```json
{"apiVersion":"v1","code":"forbidden"}
```

## Authorization and allowed capabilities

`New` requires a backend, resolver, and audit sink. The backend implements
`DurableStatePath`, `Status`, and `SwitchConditional`; the service exposes
`Handle(context.Context, []byte) Response`. `Config` contains only `Timeout` and
`Grants`. The durable path must be absolute, clean, and non-root. The timeout must
be positive and at most five minutes. Grant keys must be nonempty principals;
each `AuditID` must be 1–64 bytes of ASCII letters, digits, `-`, or `_`, and must
not be the reserved value `anonymous`. Workload grants accept only `text`, `media`,
and `idle`. Invalid configuration is rejected at construction.

The trusted `Resolver` obtains a verified principal from adapter-owned context;
the request body never carries identity. The service matches that principal to
server-configured grants. Each grant has a configured opaque audit ID, an explicit
status permission, and an allowlist of workloads. The service copies the grant map
and workload slices at construction so subsequent caller mutations cannot expand
permissions. Unknown principals and operations outside a grant fail closed without
calling the backend or disclosing state.

An authorized workload request calls only `SwitchConditional` for its permitted
target, with the configured audit ID as initiator and the supplied state token.
It cannot take user control, switch user work, return control, reconcile, recover,
resolve unfinished work, restore state, or control systemd directly. Human ownership
transfer and destructive recovery need separate explicit authority; an admin flag
must not add these operations to this automated contract.

## State tokens and atomic transitions

The expected-state token combines the store incarnation with its state version.
It is distinct from the incarnation/epoch lease fence used for execution admission.
A fresh or explicitly restored store can have the same numeric version as an old
store while belonging to a different incarnation. A version alone is insufficient.

The controller checks the token during its source read and passes it to the
authoritative store transaction. Before closing admission, inserting a transition,
or causing runtime effects, that transaction verifies both incarnation and version,
supervisor ownership, a stable phase, non-error health, and no running transition.
An observation followed by an ordinary unconditional switch cannot replace this
check. Existing CLI and human ownership operations retain their own behavior.

A successful status response is an observation, not a reservation. State can change
before the next request. A stale token requires a fresh status observation and a
new decision about the requested target.

## Concurrency and cancellation

Both status and workload requests acquire the same exclusive process gate used by
`gpu-mode`, derived from the trusted backend's `DurableStatePath()` plus `.lock`.
The path is not caller input or an independent service configuration value. The
backend must truthfully identify its exact durable store, and adapters must be
stopped and reopened when that store is restored.
A held gate returns `busy`; the service does not queue, wait, or retry. Status also
needs the gate because controller status can latch runtime observation failures;
it is not a lock-free or side-effect-free readiness query.

Calls are synchronous. The service holds the gate until the backend returns,
including rollback and finalization when those paths run. It does not return a
timeout while leaving its transition running in a background goroutine. Already canceled calls
do not execute backend operations.

The server configures a positive request timeout of at most five minutes,
respecting any earlier caller deadline. This one budget includes identity
resolution, backend execution, and both audit calls.
Cancellation during a transition can take the request budget plus the controller's
existing `CleanupTimeout` and `FinalizeTimeout` allowances when cleanup and
finalization are invoked. Cancellation does not guarantee rollback or a stable
result: interrupted final commit can leave admission closed and a transition in
progress, requiring explicit human recovery. The controller and runtime adapters
must honor their contexts; the interface cannot force a deliberately
noncooperative backend to terminate. A client timeout or canceled connection does
not establish whether a transition committed. Refresh status before deciding to
send another workload request. There is no automatic retry, deduplication, queue,
or idempotency guarantee.

## Audit and redaction

Audit events contain only `timestamp`, a server-generated UUID `correlationId`,
`auditId`, `operation`, `target`, `stage`, and `outcome`. The audit identity is a
configured opaque ID or the fixed value `anonymous`. Operations are `status`,
`request-workload`, or `invalid`; targets are `none`, `text`, `media`, `idle`, or
`invalid`; stages are `admission` and `outcome`; outcomes use the fixed result codes.
Invalid attacker values receive a fixed `invalid` classification. Raw bodies,
principal strings, identity hashes, headers, credentials, paths, arbitrary errors,
prompts, and outputs are not audit payloads.

An admitted backend call first records an `admission` event with outcome `ok`
under the process gate, then records an `outcome` event with the result code. Both
use the same correlation ID. Requests rejected before admission record only an
`outcome` event. Authentication failures use `anonymous`; malformed or unsupported
envelopes use `invalid` operation and target classifications.

Audit failure before backend execution prevents execution and returns `unavailable`.
A failure recording the result after execution cannot undo an operation: the service
returns `unavailable`, which does not assert rejection or absence of side effects.
Refresh status before retrying. Audit calls use the same capped request context,
with no separate background or deferred budget. An expired or canceled context
can prevent the sink from recording an outcome. Any outcome-audit failure overrides
the primary result with `unavailable`, including a primary `timeout` or `canceled`
result; this also does not prove there were no side effects. Resolvers and audit
sinks must honor contexts, just as backend adapters must.

## Future transport requirements

A future adapter must authenticate the transport and map verified principals on
the server before invoking the service. Never treat request-supplied identity fields
or unverified identity headers as authentication. Fail closed when verified
credentials are absent, invalid, expired, or revoked. Scope credentials to the
intended grants, support revocation, and exclude credentials from URLs and logs.
Loopback reachability alone is not identity.

Browser session or cookie adapters require an exact Origin allowlist, CSRF
protection for mutations, restrictive CORS, and no wildcard credentialed origin.
Origin checks supplement authentication. Concrete identities, credentials,
routes, broker choices, and deployment configuration remain outside this repository.

## Broker and execution boundary

The broker owns queues, workflows, job state, prompts, and outputs. Selecting a
workload neither reserves GPU capacity nor proves that a content job executed or
completed. Execution must use the existing admission/lease/proxy contract, and
completion must use the admitted correlation tuple and verified terminal evidence
described in the [execution proxy adapter contract](PROXY-CONTRACT.md). A workload
switch response cannot substitute for execution admission or terminal completion.
