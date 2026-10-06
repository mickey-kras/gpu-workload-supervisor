# Local operator protocol

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
response additionally contains `status` with:

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

Status and mutations both use the same nonblocking controller gate. `busy` means
no request was queued. Errors contain no status and use exactly:
`invalid_request`, `unsupported_version`, `incompatible_configuration`,
`stale_state`, `wrong_owner`, `busy`, `recovery_required`, `timeout`, `unavailable`.
Invalid request envelopes may have an empty correlation ID. Error responses do
not disclose profile paths or runtime output.

## Lifetimes and limits

Status defaults to 30 seconds, with a profile maximum of 60 seconds. Mutation
execution defaults to 15 minutes, with a maximum of 30 minutes. Existing failure
cleanup can add two minutes and durable finalization can add ten seconds.
Response writing has a separate two-second limit and a 64 KiB cap. Clients should
allow 75 seconds for status and 33 minutes for mutations, and age observations
from monotonic dispatch time.

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
