<a id="execution-proxy-adapter-contract"></a>

# Contract for execution proxy adapters

[Documentation](README.md) | [Repository](../README.md)

The supervisor is content-blind. Its generic tests qualify admission, correlation,
forwarding and ownership behavior. They do not qualify any runtime deployment.

Keep execution disabled until you check the exact deployed runtime version, route
inventory, and terminal completion semantics. Requalify after a runtime, adapter, proxy
or ingress change affecting this contract.

## Route inventory and migration

Each proxy needs a deployment-owned inventory with runtime version and artifact
identity, adapter version, exact HTTP method and path, classification, and verification
evidence. Configure every route the deployment exposes:

| Classification | Flag | Requirement |
| --- | --- | --- |
| Execution | `-execute-route METHOD:/path` | Any route that can start work, including GET, must use admission. |
| Read-only | `-read-only-route GET:/path` | Verified non-executing GET, HEAD or OPTIONS. Repeat per method and path. |
| Editing or monitoring mutation | `-passthrough-route METHOD:/path` | Verified not to start work, change ownership, or bypass admission. |

Routes must use canonical absolute paths: no trailing slash except `/`, repeated
slashes, or dot segments. Noncanonical decoded request paths return HTTP 400 with
`path_not_canonical`. After ingress authentication and request validation, canonical
unknown paths return HTTP 404.

Recognized paths with an unconfigured method return HTTP 405 with an `Allow` header
listing the explicitly configured methods, including POST for the completion path. Both
route rejections use the JSON error `route_not_allowed`. Neither invalid paths nor route
rejections reach upstream.

There are no implicit GET, HEAD or OPTIONS routes, wildcard routes, or automatic HEAD
classification. The proxy rejects overlapping classifications. Explicit non-execution
routes remain available while execution admission stays closed, if the upstream is
reachable.

**Migration:** earlier proxies automatically forwarded unclassified GET, HEAD and
OPTIONS. Inventory these routes and add explicit `-read-only-route` entries before
upgrading. Preserve verified editing mutations as explicit passthrough entries.

Do not restore broad forwarding to make a UI work. First check its route semantics.

Matching uses the exact method and decoded URL path. The proxy forwards query strings,
escaped path variants, bodies and message payloads without semantic inspection. Classify
the entire route by its most privileged behavior: a monitoring path that can enqueue
through a query parameter is execution, never read-only.

Check the aliases, trailing slashes, encoded paths, upstream base paths, redirects and
HTTP upgrades. Do not classify WebSocket or other upgraded channels that can submit work
as read-only monitoring. A trusted adapter must split or gate such operations when one
HTTP route cannot safely represent one admission and completion lifecycle.

Route names and HTTP method conventions are not proof of behavior.

## Admission and completion

During supervisor ownership, the proxy commits a registration before dispatch. The
trusted upstream receives the normalized request ID, admitted fence incarnation and
epoch, and fresh registration token. Keep that tuple unchanged through submission,
streaming, backend tracking and terminal confirmation. Job identifiers and backend state
interpretation belong to the adapter, not this supervisor.

New supervisor-owned request IDs must be valid UTF-8 and contain at most 8192 bytes
after the proxy removes leading and trailing whitespace. Oversized IDs return HTTP 400
with `request_id_too_long`. Invalid UTF-8 returns `request_id_invalid`, before any
registration or upstream dispatch.

This byte limit allows worst-case JSON escaping (six bytes per input byte). The escaped
ID, fence, registration token and outcome fit within the completion body limit of 64
KiB. Use the normalized ID unchanged in the callback. Avoid excessive JSON whitespace.

Existing registrations keep their completion behavior.

The callback contract uses the configured completion path (default `POST
/_gpu-workload-supervisor/v1/work/finish`):

```json
{
  "requestId": "ADMITTED_REQUEST_ID",
  "registrationToken": "ADMITTED_TOKEN",
  "fence": {"incarnation": "ADMITTED_INCARNATION", "epoch": 1},
  "outcome": "completed"
}
```

| Runtime behavior | Required terminal evidence |
| --- | --- |
| Synchronous response | Version-specific proof that execution ended. Then explicit callback. |
| Asynchronous enqueue | Correlated terminal backend state. Acceptance or job ID is insufficient. |
| Streaming response | Verified terminal execution event. EOF or disconnect alone is insufficient. |
| Rejection or cancellation | Proof the admitted work cannot still run before reporting `abandoned`. |

Execution dispatch removes `GetBody` and caller-supplied `Idempotency-Key` and
`X-Idempotency-Key` replay hints. It uses a non-rewindable body. This includes empty
execution requests and routes that use GET, HEAD, OPTIONS or TRACE.

The standard upstream transport does not automatically replay execution after an
ambiguous connection failure. The transport preserves payload bytes. It sends
known-empty HTTP/1 execution requests without chunked framing.

Read-only and passthrough routes keep ordinary connection reuse and transport behavior.
This is not a backend deduplication guarantee: clients must not resubmit ambiguous
execution under a fresh request ID.

HTTP 200, 202, error responses, streaming EOF, transport failures and cancellation never
automatically finish registrations. A callback succeeds with HTTP 204. The proxy rejects
missing or forged tokens and mismatched registered fences or workloads.

For a duplicate callback with the original matching tuple, the proxy returns HTTP 204
for either valid outcome. The first stored outcome remains unchanged. An unknown request
returns 404.

Callback responses are not independent proof of terminal execution. Completion uses the
originally registered fence, including when the current fence rotated during draining.
Old tokens cannot complete a later reuse of the ID.

The token is a completion capability, not proof that execution ended. In `trusted`
ingress mode (the default), upstream HTTP responses return it to the trusted caller.
Opt-in [adapter ingress](EXECUTION-PROXY.md#trusted-adapter-ingress) instead suppresses
it from client response metadata and requires separate execution and completion
credentials.

Only the trusted terminal adapter holds completion authority. The generic upstream
receives the tuple in both modes and must not expose it through bodies or logs. Optional
[requester ingress](REQUESTER-INGRESS.md) adds requester and workflow authorization
before durable demand and admission.

Otherwise authenticate at the deployment gateway. Direct runtime bypass prevention
remains a deployment control. Legacy tokenless registrations accept completion with the
original registered request ID and fence, even after the current fence rotates.

Rotation only lifts the pruning protection for completed legacy rows. It does not revoke
unfinished work. New adapters must use registration tokens.

## Opt-in ComfyUI asynchronous execution

`-comfyui-contract comfyui-a7169322485d0049380fb207fa17e9fb3ec40486` selects the
source-qualified lifecycle contract from `Comfy-Org/ComfyUI` commit
`a7169322485d0049380fb207fa17e9fb3ec40486`. The default configuration disables it. It
requires `-ingress-mode adapter`, separate ingress credentials, a non-native configured
workload and exactly `-execute-route POST:/prompt`.

This flag records an explicit deployment qualification assertion. It does not detect or
attest the installed runtime version. Check the exact deployed runtime and artifact and
custom nodes against the [source-derived
fixtures](../internal/proxy/testdata/comfyui/README.md) before setting it.

Requalify after changes. The adapter does not support older servers that ignore caller
`prompt_id`.

Use plain HTTP and a loopback root URL for the runtime. It must have no credentials,
query, fragment or encoded or base path. Optional monitoring routes are only
`GET:/prompt`, `GET:/queue` and `GET:/history`. The adapter does not support
passthrough, job-ID headers, other execution routes or WebSocket submission channels.

It rejects query-bearing and encoded-body requests. The trusted gateway (or optional
requester policy with an approved exact-body digest) must authorize callers and graphs.
Deployment review must validate custom nodes and input and output locations before
submitting.

JSON-envelope validation in the proxy does not prove graph safety. Automation receives a
gateway job handle, not raw ComfyUI execution access or proxy ingress credentials.
Deployment isolation must prevent direct runtime bypass for every caller and alternate
channel.

For Supervisor-owned execution the proxy generates fresh UUIDs for `prompt_id` and
`extra_data.gpu_workload_supervisor.attemptId`, replacing caller lifecycle values. In
one SQLite transaction, the proxy commits admission, original fence and token and those
UUIDs before any `POST /prompt` dispatch. The registration tuple stays inside the
supervisor.

Raw ComfyUI receives only graph data and non-secret correlation IDs. This adapter stores
no graph, input, output, job payload or artifact.

Acceptance or a transport or client disconnect leaves work unfinished. A single
process-lifetime reconciler polls `GET /history/{prompt_id}` and checks exact
case-sensitive prompt, correlation and status fields. Only `status_str: "success"` with
`completed: true`, the original prompt UUID and attemptId, followed by absence of that
prompt UUID from **both** well-formed `/queue` lists completes the original
registration.

Empty history paired with exactly one valid queued or running entry matching both UUIDs
is ordinary pending work: it remains unfinished without an error report. The following
conditions leave work unresolved and produce diagnostic reasons without tokens or
payloads:

- Error or cancellation histories or missing or pruned history with no correlated queue identity.
- Malformed or oversized replies, redirects or unavailable runtimes.
- Failed completion commits.

Success history proves execution termination under this qualified contract. It does not
prove driver VRAM release or artifact validity.

The adapter reads at most 16 MiB per envelope or reply and admits at most 4096
unfinished ComfyUI attempts. It polls at most 64 records per pass with a 10-second pass
and 2-second budget for each record. A rotating cursor prevents unresolved older
attempts from starving other work.

Shutdown cancels polling before closing durable state. User-mode forwarding retains the
shared handoff gate and runtime stop and release checks. It creates no Supervisor work
registrations.

Normal restart polls persisted UUIDs and never sends `/prompt` again. A repeated request
ID conflicts while its registration remains retained. If a response is missing,
**never** retry an ambiguous enqueue under a fresh request ID.

The proxy removes `GetBody` and idempotency replay hints from upstream submission. After
a pre-admission rejection, the proxy dispatched no execution. Get valid admission before
you submit again.

After admission, use terminal backend evidence or the verified `resolve-work` procedure
when you cannot resolve uncertainty. Start a new execution only after that disposition
proves earlier execution cannot still run. Restart or missing queue entries alone is not
such proof.

Schema v14 adds lifecycle metadata referencing `registered_work` with cascading deletion
under existing bounded completed-work retention. It preserves prior registrations,
fences, tokens, settings and outcomes. Legacy unfinished work gets no inferred runtime
identity and remains unfinished.

Older binaries reject v14. Rollback requires the verified binary and configuration and
pre-upgrade backup procedure in [restoring](RESTORING.md), never editing schema
metadata. Enabling this adapter does not change workload configuration or enable
automatic idle policy.

## Failure, restart and ownership

If either party cannot know whether work started or ended, leave it unfinished. Client
disconnects, truncated upstream streams and proxy shutdown preserve durable registration
state. Normal restart preserves its original correlation.

A trusted adapter can finish it later using terminal evidence and the original tuple. If
the token is missing or state stays ambiguous, use the verified `resolve-work` procedure
in [operations](OPERATIONS.md#ownership-and-unfinished-work). Stop all proxy instances.

Stop the runtimes. Check the release. Then record an audited abandonment.

Ordinary recovery is not proof that orphaned work finished.

User mode allows manual execution of the selected healthy workload without lease
registration. Forwarding holds the shared handoff gate. Ownership changes close
execution, stop user runtimes and wait for forwarding to exit before starting the
target.

An enqueue response ending HTTP does not prove the backend is idle: stopping and release
verification still apply. Timeout or failed handoff keeps admission closed and never
restarts stopped user work. Non-execution editing or monitoring routes must remain safe
even during this interval.

## Deployment qualification record

Record the evidence for all of the following before enabling execution:

- Exact runtime, adapter and proxy versions and reviewed method and path classifications,
  including query, body and escaped-path variants and upgraded execution channels.
- Synchronous, asynchronous and streaming terminal semantics, with durable tuple
  correlation and no early completion after acceptance or disconnection.
- Stale fences, closed admission, duplicate or forged callbacks, closure and drain ordering,
  client, upstream and proxy failures, restart, orphan resolution, and User-mode handoff.
- Deployment controls prevent direct runtime execution bypass for every relevant caller and alternate endpoint or channel. A loopback listener alone does not enforce this.
- Trusted authentication and ingress, callback access, token handling and host and network
  isolation. These are deployment responsibilities, not supervisor features.

Repository evidence: `internal/proxy/contract_integration_test.go`,
`internal/supervisor/proxy_handoff_test.go` and
`internal/supervisor/orphan_recovery_test.go`. The real-systemd qualification suite
covers runtime lifecycle and release behavior, not backend routes or job semantics.
