# Optional requester ingress

The proxy can authenticate individual requesters and authorize exact workflows
before recording demand or activating a workload. This is opt-in. Auto-idle
remains Off: the packaged operator still rejects enablement until deployment
qualification proves that direct runtime execution cannot bypass this ingress.
Loopback binding and credentials alone do not establish that isolation.

## Configuration

Add `-requester-policy-file /absolute/private/requesters.json` to the existing
`gpu-workload-proxy` adapter-mode invocation. Keep separate execution and
completion credential files. The proxy state path must match its account's
accepted operator profile. No new daemon, timer or remote listener is required.

The policy file must pass the same private-file ownership, permissions and
no-symlink checks as credential files. It is limited to 1 MiB and loaded once;
restart the proxy after changing it. Example structure:

```json
{
  "requesters": [{
    "id": "automation",
    "credential": "<64 lowercase hex characters from 32 random bytes>",
    "workflows": [{
      "id": "approved-job",
      "method": "POST",
      "path": "/prompt",
      "bodySHA256": "<64 lowercase hex characters: SHA-256 of the exact request body>"
    }]
  }]
}
```

Each workflow must name a configured execution route. The workload is the
proxy's configured workload. Requester credentials must be distinct from every
other requester and both adapter credentials. IDs are 1-64 ASCII letters,
digits, dots, underscores or hyphens. Limits: 128 requesters, 128 workflows each,
16 MiB request bodies. This first version authorizes immutable payloads; changing
inputs or whitespace requires approving their new body digest. It does not
interpret a caller-provided workflow name as approval for an arbitrary graph.

Requester mode excludes read-only and passthrough routes. The shared execution
credential cannot bypass requester policy. Only the completion credential can
call the existing terminal endpoint; requesters never receive completion tokens.
Neither requester credentials nor workflow headers are forwarded to runtimes.

## Requests and cancellation

Send the approved body to its execution route with:

- `X-Workload-Requester-Credential`: the requester's credential.
- `X-Workload-Workflow`: the approved workflow ID.
- `X-Request-ID`: a unique demand ID (or the configured request-ID header).

The gateway supplies the current lease fence. Unauthorized requests, mismatched
bodies, duplicate identity headers, query strings, encoded bodies and trailers
are rejected before demand is recorded. Credentials and bodies are never stored;
durable evidence contains requester/workflow IDs and the body digest.

A healthy Supervisor-owned Idle workload activates through the existing typed
operator action, waits for readiness and admits only with the resulting fence.
A different active workload or transition returns HTTP 409 `demand_queued` and
keeps durable demand. It never interrupts work or forces an active workload into
Idle. The caller retains its payload and explicitly retries the same request
when the supervisor can accept it; there is no background replay worker.

To cancel a queued demand, send **DELETE to the same execution path**, with the
same credential, workflow, request ID and approved body. Success returns 204.
Cancellation cannot terminate a reservation or admitted work. Cancel deferred
requests before removing their credentials or workflow policy.

Once reserved/admitted, duplicate requests return `demand_conflict` rather than
replaying execution. A process crash, disconnect or ambiguous dispatch leaves
evidence unresolved. Verified operator recovery must quiesce proxies, verify
runtimes stopped and rotate the fence before resolving it. No timer silently
expires unfinished demand. Backup restoration retires reserved demand and its
claims atomically with the old lease; queued intent remains available for explicit
retry or cancellation. Existing completed-work retention also removes old
cancelled/terminal demand records. Queued and reserved demand together are capped
at 4096 and retained records at 65536; reaching capacity rejects new work.

## Durable evidence

Queue insertion, reservation and work registration are SQLite transactions.
Admission consumes the reservation claim in the same transaction as the existing
native or ComfyUI work registration. Queued/reserved demand invalidates pending
idle deadlines and is checked in the atomic idle decision. Admitted work remains
pending until trusted terminal evidence or verified operator recovery completes
it. Existing native streaming and ComfyUI reconciliation contracts still apply.

This evidence does not attest to external jobs that bypass the gateway. No
qualified `EvidenceProvider` is installed, and auto-idle enablement remains
unavailable. Host runtime isolation and terminal/lifecycle qualification are a
separate deployment step; fixture tests do not satisfy that requirement.
