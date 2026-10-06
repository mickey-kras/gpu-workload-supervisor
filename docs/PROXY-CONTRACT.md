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
A duplicate terminal callback returns HTTP 404 and cannot change the first outcome;
an unknown request also returns 404, so it is not independent proof of completion.
Completion uses the originally registered fence, including when the current fence
has rotated during draining. Old tokens cannot complete a later reuse of the ID.

The token is a completion capability, not proof that execution ended. The upstream
receives it and HTTP responses received from upstream return it to the caller. Only trusted
parties with verified terminal evidence may use it. Protect callback access at the
deployment boundary; never expose it or its tokens to untrusted clients or logs.
Legacy tokenless registrations accept completion with the original registered
request ID and fence, even after the current fence rotates. Rotation only lifts
the pruning protection for completed legacy rows; it does not revoke unfinished
work. New adapters must use registration tokens.

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
