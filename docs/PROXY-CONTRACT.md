# Execution proxy adapter contract

[Documentation](README.md) | [Repository](../README.md)

The supervisor is content-blind. Its generic tests qualify admission, correlation,
forwarding and ownership behavior; they do not qualify any runtime deployment.
Keep execution disabled until the exact deployed runtime version, route inventory,
and terminal completion semantics have been verified. Requalify after a runtime,
adapter, proxy or ingress change affecting this contract.

## Route inventory and migration

Each proxy needs a deployment-owned inventory with runtime version/artifact identity,
adapter version, exact HTTP method/path, classification, and verification evidence.
Configure every route the deployment exposes:

| Classification | Flag | Requirement |
| --- | --- | --- |
| Execution | `-execute-route METHOD:/path` | Any route that can start work, including GET, must use admission. |
| Read-only | `-read-only-route GET:/path` | Verified non-executing GET, HEAD or OPTIONS; repeat per method/path. |
| Editing/monitoring mutation | `-passthrough-route METHOD:/path` | Verified not to start work, change ownership, or bypass admission. |

Routes must use canonical absolute paths: no trailing slash except `/`, repeated
slashes, or dot segments. Noncanonical decoded request paths return HTTP 400 with
`path_not_canonical`; canonical but unclassified routes return HTTP 405. Neither
reaches upstream. There are no implicit
GET, HEAD or OPTIONS routes, wildcard routes, or automatic HEAD classification.
Overlapping classifications are rejected. Explicit non-execution routes remain
available while execution admission is closed, if the upstream is reachable.

**Migration:** earlier proxies automatically forwarded unclassified GET, HEAD and
OPTIONS. Inventory these routes and add explicit `-read-only-route` entries before
upgrading. Preserve verified editing mutations as explicit passthrough entries.
Do not restore broad forwarding to make a UI work; first verify its route semantics.

Matching uses the exact method and decoded URL path. Query strings, escaped path
variants, bodies and message payloads are forwarded without semantic inspection.
Classify the entire route by its most privileged behavior: a monitoring path that
can enqueue through a query parameter is execution, never read-only. Verify aliases,
trailing slashes, encoded paths, upstream base paths, redirects and HTTP upgrades.
WebSocket or other upgraded channels that can submit work cannot be treated as
read-only monitoring. A trusted adapter must split or gate such operations when one
HTTP route cannot safely represent one admission/completion lifecycle. Route names
and HTTP method conventions are not proof of behavior.

## Admission and completion

During supervisor ownership, the proxy commits a registration before dispatch.
The trusted upstream receives the normalized request ID, admitted fence incarnation
and epoch, and fresh registration token. Retain that tuple unchanged through
submission, streaming, backend tracking and terminal confirmation. Job identifiers
and backend state interpretation belong to the adapter, not this supervisor.

New supervisor-owned request IDs must be valid UTF-8 and contain at most 8192
bytes after leading/trailing whitespace is removed. Oversized IDs return HTTP 400
with `request_id_too_long`; invalid UTF-8 returns `request_id_invalid`, before any
registration or upstream dispatch. This byte limit allows even worst-case JSON
escaping (six bytes per input byte) to fit the 64 KiB completion body limit with
the fence, registration token and outcome. Use the normalized ID unchanged in
the callback; avoid excessive JSON whitespace. Existing registrations retain
their completion behavior.

The callback contract uses the configured completion path (default
`POST /_gpu-workload-supervisor/v1/work/finish`):

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
| Synchronous response | Version-specific proof that execution has terminated; then explicit callback. |
| Asynchronous enqueue | Correlated terminal backend state; acceptance or job ID is insufficient. |
| Streaming response | Verified terminal execution event; EOF or disconnect alone is insufficient. |
| Rejection/cancellation | Proof the admitted work cannot still run before reporting `abandoned`. |

HTTP 200, 202, error responses, streaming EOF, transport failures and cancellation
never automatically finish registrations. A callback succeeds with HTTP 204.
Missing/forged tokens and mismatched registered fences/workloads are rejected.
A duplicate callback with the original matching tuple returns HTTP 204 for either
valid outcome; the first stored outcome remains unchanged. An unknown request
returns 404; callback responses are not independent proof of terminal execution.
Completion uses the originally registered fence, including when the current fence
has rotated during draining. Old tokens cannot complete a later reuse of the ID.

The token is a completion capability, not proof that execution ended. In default
`trusted` ingress mode, upstream HTTP responses return it to the trusted caller.
Opt-in [adapter ingress](EXECUTION-PROXY.md#trusted-adapter-ingress) instead suppresses
it from client response metadata and requires separate execution/completion
credentials. Only the trusted terminal adapter holds completion authority. The
generic upstream receives the tuple in both modes and must not expose it through bodies
or logs. Independently protect requester authentication and direct runtime bypass
at the deployment boundary; adapter credentials do not replace those controls.
Legacy tokenless registrations accept completion with the original registered
request ID and fence, even after the current fence rotates. Rotation only lifts
the pruning protection for completed legacy rows; it does not revoke unfinished
work. New adapters must use registration tokens.

## Opt-in ComfyUI asynchronous execution

`-comfyui-contract comfyui-a7169322485d0049380fb207fa17e9fb3ec40486`
selects the source-qualified lifecycle contract from Comfy-Org/ComfyUI commit
`a7169322485d0049380fb207fa17e9fb3ec40486`. It is disabled by default and requires
`-ingress-mode adapter`, separate ingress credentials, a non-native configured
workload and exactly `-execute-route POST:/prompt`. This flag records an explicit
deployment qualification assertion; it does not detect or attest the installed
runtime version. Verify the exact deployed runtime/artifact and custom nodes
against the [source-derived fixtures](../internal/proxy/testdata/comfyui/README.md)
before setting it. Requalify after changes. Older servers that ignore caller
`prompt_id` are unsupported.

Use a plain loopback runtime root URL with no credentials, query, fragment or
encoded/base path. Optional monitoring routes are only `GET:/prompt`,
`GET:/queue` and `GET:/history`. Passthrough, job-ID headers, other execution
routes and WebSocket submission channels are unsupported. Query-bearing and
encoded-body requests are rejected. The trusted gateway must authenticate callers
and validate allowed graphs/custom nodes and input/output locations before
submitting; JSON-envelope validation in the proxy does not establish graph safety.
Automation receives a gateway job handle, not raw ComfyUI execution access or
proxy ingress credentials. Deployment isolation must prevent direct runtime bypass
for every caller and alternate channel.

For Supervisor-owned execution the proxy generates fresh UUIDs for `prompt_id`
and `extra_data.gpu_workload_supervisor.attemptId`, replacing caller lifecycle
values. One SQLite transaction commits admission, original fence/token and those
UUIDs before any `POST /prompt` dispatch. The registration tuple stays inside the
supervisor; raw ComfyUI receives only graph data and non-secret correlation IDs.
No graph, input, output, job payload or artifact is stored by this adapter.

Acceptance or a transport/client disconnect leaves work unfinished. A single
process-lifetime reconciler polls `GET /history/{prompt_id}` and checks exact
case-sensitive prompt/correlation/status fields. Only `status_str: "success"`
with `completed: true`, the original prompt UUID and attemptId, followed by
absence of that prompt UUID from **both** well-formed `/queue` lists completes
the original registration. Empty history paired with exactly one valid queued or
running entry matching both UUIDs is ordinary pending work: it remains unfinished
without an error report. Error/cancellation histories, missing/pruned history with
no correlated queue identity,
malformed/oversized replies, redirects, unavailable runtimes and failed completion
commits stay unresolved and produce diagnostic reasons without tokens/payloads.
Success history proves execution termination under this qualified contract;
it does not prove driver VRAM release or artifact validity.

The adapter reads at most 16 MiB per envelope/reply, admits at most 4096 unfinished
ComfyUI attempts, and polls at most 64 records per pass with a 10-second pass and
2-second per-attempt budget. A rotating cursor prevents unresolved older attempts
from starving other work. Shutdown cancels polling before closing durable state.
User-mode forwarding retains the shared handoff gate and runtime stop/release
checks; it creates no Supervisor work registrations.

Normal restart polls persisted UUIDs and never sends `/prompt` again. A repeated
request ID conflicts while its registration remains retained; an ambiguous enqueue
must **never** be retried under a fresh request ID merely because a response was
lost. `GetBody` and idempotency replay hints are removed from upstream submission.
After a pre-admission rejection no execution was dispatched; obtain valid admission
before submitting again. After admission, use terminal backend evidence or the
verified `resolve-work` procedure when uncertainty cannot be resolved. A fresh
attempt is allowed only after that disposition proves earlier execution cannot
still run. Restart or missing queue entries alone is not such proof.

Schema v14 adds lifecycle metadata referencing `registered_work` with cascading
deletion under existing bounded completed-work retention. It preserves prior
registrations, fences, tokens, settings and outcomes. Legacy unfinished work gets
no inferred runtime identity and remains unfinished. Older binaries reject v14;
rollback requires the verified binary/configuration and pre-upgrade backup procedure
in [restoring](RESTORING.md), never editing schema metadata. Enabling this adapter
does not change workload configuration or enable automatic idle policy.

## Failure, restart and ownership

If either party cannot determine whether work started or ended, leave it unfinished.
Client disconnects, truncated upstream streams and proxy shutdown preserve durable
registration state. Normal restart preserves its original correlation. A trusted
adapter may finish it later using terminal evidence and the original tuple. If the
token was lost or state remains ambiguous, use the verified `resolve-work` procedure
in [operations](OPERATIONS.md#ownership-and-unfinished-work): stop all proxy instances, stop runtimes, verify release, then record
an audited abandonment. Ordinary recovery is not proof that orphaned work finished.

User mode allows manual execution of the selected healthy workload without lease
registration. Forwarding holds the shared handoff gate. Ownership changes close
execution, stop user runtimes and wait for forwarding to exit before starting the
target. An enqueue response ending HTTP does not prove the backend is idle: stopping
and release verification still apply. Timeout or failed handoff keeps admission
closed and never restarts stopped user work. Non-execution editing/monitoring routes
must remain safe even during this interval.

## Deployment qualification record

Record evidence for all of the following before enabling execution:

- Exact runtime/adapter/proxy versions and reviewed method/path classifications,
  including query/body/escaped-path variants and upgraded execution channels.
- Synchronous, asynchronous and streaming terminal semantics, with durable tuple
  correlation and no early completion after acceptance or disconnection.
- Stale fences, closed admission, duplicate/forged callbacks, closure/drain ordering,
  client/upstream/proxy failures, restart, orphan resolution, and User-mode handoff.
- Direct runtime execution bypass is prevented for every relevant caller and
  alternate endpoint/channel. A loopback listener alone does not enforce this.
- Trusted authentication/ingress, callback access, token handling and host/network
  isolation. These are deployment responsibilities, not supervisor features.

Repository evidence: `internal/proxy/contract_integration_test.go`, `internal/supervisor/proxy_handoff_test.go` and `internal/supervisor/orphan_recovery_test.go`; the real-systemd qualification suite covers runtime lifecycle and release behavior, not backend routes or job semantics.
