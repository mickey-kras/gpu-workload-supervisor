# Architecture and integration

[Documentation](README.md) | [Deployment](DEPLOYMENT.md)

- [Responsibilities and state](#state-and-responsibility)
- [Configure workloads](#configured-workloads) and [native bindings](#native-model-bindings)
- [Configure the proxy](#configure-the-execution-proxy), [credentials](#trusted-adapter-ingress), and [limits](#connection-limits)
- [Qualify an adapter](#contract-for-execution-proxy-adapters), including [ComfyUI](#opt-in-comfyui-asynchronous-execution)
- [Owned-unit backstops](#orchestrator-decision)
- [Resource-limit design](RESOURCE-LIMITS.md): catalog v5 accepts the schema. Units with limits do not start yet

## State and responsibility

### Boundaries

| Part | Responsibility and boundary |
| --- | --- |
| Controller | Ownership, desired state, admission, fences, transitions, journal, recovery and verified GPU release. Control metadata only. No prompts or responses |
| Proxy and adapters | Route gates, forwarding, bounded protocol checks, exact-body authorization, optional requester authentication, work registration and terminal evidence. No semantic content policy |
| External broker | Workflow steps, retries, checkpoints, cancellation and results |
| External gateway | Authentication, request and response policy and content validation |
| External ingestion tools | Model scans, reports, signing and promotion |
| Trainer adapter | Recipes and checkpoint contents. The controller owns finite-job lifecycle |

Demand records prove admission. They do not schedule workflows or replay uncertain execution. Integrations must preserve admission, terminal evidence and runtime-bypass prevention. Setup installs no broker, gateway or scanner. See [requester ingress](OPERATIONS.md#optional-requester-ingress) and [training](TRAINING.md#finite-training-jobs).

### Model checks

The release has no model checks. [#585](https://github.com/mickey-kras/gpu-workload-supervisor/issues/585) plans a scan before first LLM use. [#583](https://github.com/mickey-kras/gpu-workload-supervisor/issues/583) plans result display and rejection of failed models. These checks cannot prove model safety.

### State model

A new store requires reconciliation:

```text
owner=supervisor
desiredWorkload=idle
activeWorkload=unknown
phase=reconciling
health=healthy
admission=closed
```

A fence combines a store-incarnation UUID and monotonic epoch. A fresh store creates an incarnation. Explicit restore rotates it.

Restart preserves it. Transitions rotate the epoch before draining. SQLite uses WAL, full synchronous writes, foreign keys and one connection.

The journal records side-effect intent and observation.

### State writers

| Writer | Responsibility |
| --- | --- |
| `gpu-mode` | Transitions |
| `gpu-workload-proxy` | Admission and completion |
| `gpu-operator` | Embedded supervisor transitions |
| `gpu-setup` | Accepted catalog |

Any writer can migrate an opened database. Make a backup before changing binaries.

### Clients

GNOME source: `clients/gnome/`. See [installation](OPERATIONS.md#local-gnome-desktop-installation) and [protocol](OPERATIONS.md#gnome-shell-extension-client).

### Overview assets

Regenerate README light and dark SVGs. Do not edit them manually:

```sh
python3 scripts/architecture-overview.py
```

Match `internal/{proxy,supervisor,store,runtime,control,operator}` topology.

## Configured workloads

An enabled catalog contains 1-32 profiles:

```json
{
  "version": 1,
  "profiles": [
    {
      "id": "speech",
      "label": "Speech",
      "adapter": "systemd",
      "unit": "speech.service",
      "cgroup": "/user.slice/user-1000.slice/user@1000.service/app.slice/speech.service",
      "healthURL": "http://127.0.0.1:9000/health",
      "requiredMiB": 4096,
      "bootPolicy": "stop-to-idle"
    }
  ]
}
```

| Version | Supported content |
| --- | --- |
| 1 | Workload profiles and native model bindings |
| 2 | Version 1 plus owned native launches |
| 3 | Reserved. Rejected |
| 4 | Finite trainer bindings and fenced jobs. Supports owned launches |
| 5 | Version 4 plus `limits` on owned launches. Units with limits do not start yet |
| Later | Rejected until a separate feature adds support |

Unchanged v1/v2 profiles need no migration. Owned launches need v2/v4/v5. Trainers need v4/v5. [Resource `limits`](RESOURCE-LIMITS.md) need v5. Catalog, database and upstream API versions are independent. The validator rejects unsupported versions and fields outside their version.

Setup writes the lowest version the content needs: 5 with limits, 4 with trainers, 2 with owned launches, 1 otherwise. Setup cannot write limits yet: it rejects requests with limits and accepts requests that remove them. Controller, host cap and cgroup readback checks are not in this release. The supervisor refuses switches to units with limits before it stops the current workload. Reconciliation stops running units with limits and goes idle. This gate does not change other workloads.

Setup removal of the last application writes `{"version":1,"profiles":[],"disabled":true}`. Empty enabled catalogs and disabled catalogs with profiles are invalid. Adding an application clears `disabled` and restores normal requirements.

| Field or input | Constraint |
| --- | --- |
| ID | At most 64 lowercase ASCII letters, digits, `_` or `-`. Starts with a letter. The catalog reserves `idle` and `unknown` |
| Label | At most 80 characters and 128 UTF-8 bytes. Visible non-whitespace text. No control or bidirectional override or isolation characters |
| Unit and cgroup | Unique units and absolute, clean, non-root, non-overlapping cgroups |
| Endpoint | Loopback HTTP(S). No credentials or fragments |
| JSON | No unknown fields, duplicate keys or trailing values |
| Size | 256 KiB canonical input. Only valid derived owned conflicts can use another 96 KiB. Full file limit: 352 KiB |

`systemd` stops the unit and verifies recursive cgroup release. Compatibility `media-unload` additionally requires `releaseURL`. HTTP unload success never proves GPU release. A live opposing unload runtime blocks admission until independently stopped. The controller checks every opposing runtime before target start.

`requiredMiB` optionally enables NVIDIA capacity checks with trusted `-nvidia-smi` and optional headroom. It is not an enforced ceiling. Default `bootPolicy: "stop-to-idle"` stops at reconciliation. `retain` preserves an already running healthy workload without starting it.

Multiple running `retain` profiles fail closed. Healthy retained registrations keep tokens, fence and admission. Legacy behavior uses `retain` for Text and `stop-to-idle` for Media.

Accept configuration under the controller gate. Examples abbreviate the [pinned absolute executable](DEPLOYMENT.md#controller-configuration):

```sh
gpu-mode -state /absolute/state.db -catalog /absolute/workloads.json configure
```

Supply the returned opaque revision as `-configuration-revision REVISION` on updates. Successful updates atomically store catalog, history and revision and advance state version. Stale updates fail. Updates cannot remove or change profiles referenced by active or desired state, unfinished work or unfinished transitions. Reach safe Idle before initial adoption of live legacy state.

Runtime commands use the durable snapshot, never an implicit catalog-file reload:

```sh
gpu-mode -state /absolute/state.db -systemctl /usr/bin/systemctl reconcile
gpu-mode -state /absolute/state.db -systemctl /usr/bin/systemctl -target speech switch
gpu-mode -state /absolute/state.db -systemctl /usr/bin/systemctl status
```

Flags precede the command. Select catalog targets through `switch` with `-target`. Proxies accept configured IDs. SQLite checks the accepted catalog and active allocation. Adding workloads never expands existing principal grants.

Database schema v11 transactionally rebuilds workload-constrained tables, preserving state, fences, registrations, tokens, transitions, events and relationships. Older binaries reject it. [Rollback](OPERATIONS.md#make-a-backup-restore-and-roll-back-state) requires a compatible verified backup, binary and configuration. Never edit schema metadata or downgrade a live database.

### Native model bindings

A `systemd` profile can bind `nativeModel`: `runtime: "vllm"`, `instance`, exact case-sensitive API `model`, loopback base `endpoint`, absolute `launchFile` and `launchSHA256`. Each workload needs an existing unit and non-overlapping cgroup. Different models can use separate units on one runtime endpoint.

Switching stops the old unit and verifies release before starting the next. Reselecting an active workload does not restart it. Opposing units remain reconciled.

Readiness queries `/health` and `/v1/models` and requires exactly the selected model. For catalog changes, follow the [proxy drain and restart procedure](DEPLOYMENT.md#migrate-release-checks).

| Launch item | Qualification |
| --- | --- |
| Source | Matches the systemd fragment. No pending reload. Supported direct-command subset. Supported drop-ins remain unchanged and hash-checked. Ambiguous or unsupported drop-ins fail |
| Paths | Executables, scripts, sources, validated environment and data paths and every traversed ancestor belong to the desktop user or root. No world write. Group write requires user membership. Symlinks receive the same checks. Broken or excessive chains fail |
| Commands | No shell wrappers, environment files, command substitution, uninspected hooks or filesystem remapping. Bounded direct `ExecStartPre` system checks receive inspection only |
| Model | Existing local vLLM directory. This proves existence and type, not model ownership, permissions or safety |
| Files | Setup neither changes nor installs application and model files |

This single-admin policy trusts the user's groups. It does not isolate workloads from members who can change them.

`Environment=` accepts literal values, including a quoted whole assignment. HOME, cache, temporary-directory and token paths must exist and pass trust checks. Setup never reads tokens. `PATH`, `LD_LIBRARY_PATH` and `PYTHONPATH` need trusted absolute directories without empty entries.

Semicolons also separate `LD_LIBRARY_PATH`. Nonempty `LD_PRELOAD`, `LD_AUDIT` and `PYTHONHOME` remain unsupported. Other literal variables remain unchanged without behavior certification.

Unsupported expansion and environment files fail.

Directory checks do not certify loaded module and library contents, ownership, permissions, dependency resolution or plugins. The operator owns installation trust. Setup checks effective loaded assignments and preserves source files. It neither loads libraries nor removes required settings to pass.

Literal or `%h` `WorkingDirectory=` uses the same directory, symlink and ancestor checks. Exact loaded paths must match at setup and start.

Supported existing settings are `Wants=network-online.target`, `KillSignal=SIGINT`, `NoNewPrivileges=true`, `PrivateTmp=true` or `false`, `ProtectHome=read-only` and `ProtectSystem=strict`. Setup and execution compare loaded properties with preserved sources. `Wants=` is additive. Empty assignments do not clear dependencies.

Other targets fail. Compatible syntax does not prove the application works inside the sandbox. Existing `Type=` and `Restart=` remain, including `Restart=on-failure`.

Account for automatic restarts outside admission.

ComfyUI supports `--base-directory`, `--output-directory`, `--temp-directory`, `--user-directory`, `--extra-model-paths-config` and `--enable-manager`. Directories must exist and pass trust checks. Explicit configuration files must pass trust checks and be readable regular files.

Symlinks receive the same checks. Recognized endpoint and path flags accept separate values or `--option=value`, but no abbreviations or endpoint-changing TLS flags. The endpoint must use loopback HTTP.

`--cuda-device`, `--default-device` and their abbreviations fail because they override inspected GPU selection. Preserve repeated options and multiple files, up to 256 arguments and 32 configuration files per option. Other arguments remain without behavior certification.

Setup checks the configuration metadata, not YAML or referenced model or custom-node contents. Manager inspection preserves the argument under the trusted-principal script model. It never runs or imports plugins or certifies downloads, scheduled installs, plugins or models.

Deployment must protect management endpoints and direct runtime access. Execution checks exact arguments and source fingerprints. See upstream [arguments](https://github.com/Comfy-Org/ComfyUI/blob/master/comfy/cli_args.py) and [startup](https://github.com/Comfy-Org/ComfyUI/blob/master/main.py).

`CUDA_VISIBLE_DEVICES` must contain one full physical GPU UUID, stored as `gpuUUID`. Preflight, startup, readiness and release compare it with the configured NVIDIA GPU. Numeric indices, prefixes, multiple devices and MIG mappings fail.

Setup never rewrites the setting to pass. Numeric visibility leaves the installation installed but GPU identity unresolved and adoption blocked. NVIDIA indices are not CUDA ordinals.

One device at index zero, missing `CUDA_DEVICE_ORDER` in filtered metadata or adding an ordering setting does not prove mapping. Unit metadata omits possible inherited and environment-file overrides. Setup neither has same-context CUDA enumeration nor launches a runtime to infer it.

To qualify numeric visibility, approve an external service change to the configured full UUID, reload the user manager, and refresh the binding. Setup preserves the original files. See [CUDA visibility](https://docs.nvidia.com/cuda/cuda-programming-guide/05-appendices/environment-variables.html), [NVIDIA indices](https://docs.nvidia.com/deploy/nvidia-smi/index.html), and [systemd environment sources](https://www.freedesktop.org/software/systemd/man/latest/systemd.exec.html).

Owned launches can set absolute `owned.executable`, which must identify the runtime and pass executable trust checks. Omission keeps `/usr/bin`. Model selection, a side-effect-free preview and explicit activation remain required.

Native proxies need a matching upstream, supported inference route and exact JSON `model`. They reject lifetime overrides, ambiguous keys, query parameters and encoded bodies. Catalog revision changes stop new admission. Supervisor-owned native execution observes terminal response evidence:

| Response | Terminal evidence |
| --- | --- |
| OpenAI chat and completion stream | `data: [DONE]` |
| Non-streaming completion | Every choice has non-null `finish_reason` |
| Embeddings | Complete result body parses |

EOF, disconnects, errors and ambiguous bodies leave work unfinished. External completion remains first-come. Direct native-port access bypasses enforcement and must stay outside managed execution. Real GPU, systemd and runtime host qualification remains mandatory. HTTP and lifecycle fixtures are insufficient.

### ComfyUI execution completion

Non-native workloads can use the [asynchronous adapter](#opt-in-comfyui-asynchronous-execution) without a catalog version change. It adds durable correlation, not launch discovery, graph storage, scheduling or auto-idle. Existing lifecycle, readiness, drain and release checks remain. Separately qualify ingress, graph validation and runtime isolation.

## Configure the execution proxy

Qualify the [adapter contract](#contract-for-execution-proxy-adapters) before enabling execution. Explicit routes distinguish gated execution from non-execution forwarding:

```sh
/PINNED/RELEASE/gpu-workload-proxy \
  -state /PRIVATE/DURABLE/STATE/state.db \
  -listen 127.0.0.1:LISTEN_PORT \
  -upstream http://127.0.0.1:PORT \
  -workload media \
  -execute-route POST:/execute \
  -read-only-route GET:/monitor \
  -completion-path /_gpu-workload-supervisor/v1/work/finish
```

### Admission

Supervisor-owned execution requires a unique request ID, current lease incarnation and epoch, stable compatible workload, healthy state and open admission. Registration commits before dispatch.

| Header | Configuration |
| --- | --- |
| `X-Request-ID` | `-request-id-header` |
| Job correlation header | `-job-id-header` |
| `X-Workload-Lease-Incarnation` | `-fence-id-header` |
| `X-Workload-Lease-Epoch` | `-fence-epoch-header` |
| `X-Workload-Registration-Token` | Fresh proxy-generated completion capability |

The proxy strips the caller control headers, then adds the normalized admitted ID, fence and token after hop-by-hop filtering. `Connection` nominations cannot remove that tuple. Read-only, passthrough and user-owned execution strip the control headers and register no work.

New request IDs under supervisor ownership must be valid UTF-8 and at most 8192 bytes after trimming outer whitespace. Invalid IDs return HTTP 400 `request_id_invalid`. Oversize IDs return `request_id_too_long`, before registration or dispatch.

Six-byte worst-case JSON escaping still fits the ID, fence, token and outcome in the 64 KiB completion body. Preserve the normalized ID and avoid excessive JSON whitespace. Existing registrations keep their completion behavior.

### Report completion

Submission does not finish work. Trusted upstreams or callers report verified terminal evidence using the original ID, token and fence:

```json
{
  "requestId": "REQUEST_ID",
  "registrationToken": "TOKEN_FROM_ADMISSION",
  "fence": {
    "incarnation": "LEASE_INCARNATION",
    "epoch": 1
  },
  "outcome": "completed"
}
```

Default path: `POST /_gpu-workload-supervisor/v1/work/finish`. Success returns 204. Unknown IDs return 404.

Missing or forged tokens or mismatched registered fences or workloads fail. Matching duplicate callbacks return 204 for either valid outcome. The first durable outcome wins, including `abandoned`.

Use the original fence even after rotation. Old tokens cannot finish a reused ID. A capability or callback response does not prove execution ended.

In default `trusted` mode, the proxy returns tokens to callers in `X-Workload-Registration-Token`. [Adapter mode](#trusted-adapter-ingress) suppresses client metadata and separates credentials. Legacy tokenless registrations accept their original ID and fence after rotation. New adapters must use tokens.

Native proof passes to a background persistence worker without blocking response forwarding. Terminal bytes alone do not prove a durable write. A trusted callback can finish first.

Matching native retries preserve that result. Workers retry only the original ID and workload and fence and token, never execution: at most three attempts, 3 seconds each, within 10 seconds total. Permanent tuple rejection stops retries.

Logs contain fixed reason codes `retrying`, `rejected`, `exhausted` or `canceled` and `attempts`, not raw storage errors or request metadata.

Proof handed off before disconnect survives it. Rejection, exhaustion or cancellation leaves work unfinished unless another trusted completion won. EOF and read errors and disconnects alone are not terminal proof. See [shutdown](#shutdown) for cancellation that exceeds its budget.

### Retention

Default `-completed-work-retention` is 30 days. Pruning removes completed rows and transition snapshot links, not active work or in-progress transition snapshots. Pruned IDs can recur under the same fence.

Tokens prevent a stale completion. Completed legacy tokenless rows remain protected until fence rotation. Rotation does not revoke unfinished legacy completion.

### Route and listener boundary

Use loopback only: user ownership bypasses lease registration, state-directory writers can drive user execution, and the service trusts its UID. [Requester ingress](OPERATIONS.md#optional-requester-ingress) adds local identity, workflow authorization and durable demand, not host isolation. External exposure, runtime-bypass prevention and content policy remain deployment responsibilities. See [security](../SECURITY.md).

| Route class | Flag | Required evidence |
| --- | --- | --- |
| Execution | `-execute-route METHOD:/path` | Any work-starting operation, including GET |
| Read-only | `-read-only-route GET:/path` | Non-executing GET, HEAD or OPTIONS, configured separately |
| Editing and monitoring mutation | `-passthrough-route METHOD:/path` | Cannot start work, change ownership or bypass admission |

Classifications cannot overlap. No wildcards, implicit safe methods or automatic HEAD exist. Before upgrading implicit GET and HEAD and OPTIONS forwarding, inventory routes and add explicit entries, including safe editing mutations. Do not restore broad forwarding for UI compatibility.

After authentication and validation, noncanonical decoded paths return 400 `path_not_canonical`. Canonical unknown paths return 404 `route_not_allowed`. Recognized paths with unconfigured methods return 405 with the same error and configured methods in `Allow`, including completion POST.

Canonical paths are absolute, with no repeated slashes, dot segments or trailing slash except `/`. Rejected routes never reach upstream. Explicit non-execution routes remain available with admission closed if upstream responds.

### Trusted adapter ingress

Enable `-ingress-mode adapter -execution-credential-file /PRIVATE/execution.key -completion-credential-file /PRIVATE/completion.key`. Each absolute clean path must identify a private regular file owned by the proxy UID under trusted directories. No symlinks or group or other permissions.

Generate independent 32-byte random credentials as 64 lowercase hex characters. The parser accepts one final newline. Startup reads once with a bound and rejects missing, malformed, equal or unsafe files, or credential files in `trusted` mode.

Rotate by private replacement and proxy restart. Registration tuples persist.

The authenticated gateway sends `X-Workload-Execution-Credential` for execution, read-only and passthrough requests. Only the terminal adapter sends `X-Workload-Completion-Credential` for completion POSTs. Missing, wrong, duplicate or combined credentials return 401 before parsing, state access or forwarding. Execution credentials cannot finish work even with its tuple. Completion still requires terminal proof.

Adapter mode strips ingress credentials, `Authorization` and `Proxy-Authorization` from upstream headers and trailers. URL credentials are forbidden. Admitted correlation reaches trusted upstreams.

Client headers, informational responses and trailers suppress tokens and credentials, including unrelated metadata names and values containing them. Matching is case-insensitive substring matching, covering hex and UUID case changes, not other transformations. Backends and adapters must never echo secrets in bodies and logs.

Payloads remain uninspected. Native terminal observation remains internal and unchanged.

This mode is local, not a remote gateway. Without requester ingress, the gateway must authenticate requesters and enforce identity, CSRF, policy and appropriate limits. Keep execution credentials from clients, completion credentials only at the adapter, host service identities isolated and direct-runtime access blocked. Credentials alone do not qualify deployment. Idle stays Off pending separate evidence and host qualification.

### Connection limits

Private upstream and health transports connect directly, ignoring `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY`. Keep upstream trusted. Go callers' custom transports must preserve capability isolation and own their response-header timeout policy.

| Limit | Default and behavior |
| --- | --- |
| `-read-header-timeout` | 10 seconds |
| `-idle-timeout` | 2-minute idle keep-alive |
| `-max-inflight` | 128 ordinary requests. Positive |
| `-max-completion-inflight` | 16 separate completion POST slots. Positive |
| Dial and TLS handshake | 30 seconds / 10 seconds |
| `-upstream-response-header-timeout` | Positive. 10 minutes after complete upload. Set the timeout above model-load and non-streaming time, for example `20m` |
| Request bodies and response streams | No total read or write deadline |

Saturation returns 503 `{"error":"proxy_capacity_exceeded"}` with `Retry-After: 1` before registration and forwarding. This suggests a delay, not capacity assurance. Non-POST completion-path requests use ordinary slots.

Streams keep HTTP slots until handlers return. Header timeout returns 502 and releases the ordinary slot. Cancellation or shutdown cancels upstream.

Probes keep their separate `-health-timeout`. None proves terminal work or clears registrations.

Supervisor-native execution also reserves one of `-max-inflight` native slots before registration. It holds that reservation until an unproven response ends or persistence finishes. Pending persistence can reject new work with zero HTTP requests.

A stream still open after persistence retains its HTTP slot. Size for streams and write delays. Completion callback slots remain separate.

Caps bound neither idle connections nor bytes per stream: use trusted clients or authenticated front-proxy connection and body and rate limits.

### Shutdown

New requests return 503 `proxy_shutting_down` without `Retry-After`. Await service availability. Default `-shutdown-timeout=30s` gives separate full budgets to graceful HTTP drain and then native worker drain. Signals and listener failure stop admission, drain producers, then seal proof submission, allowing terminal proof during HTTP drain.

Native timeout cancels retries, but shutdown joins active workers before closing SQLite or releasing the shared recovery-exclusion lock. SQLite busy handling can exceed cancellation. Custom failure callbacks from Go callers must return promptly.

This is not a hard process deadline. Service stop grace must cover both budgets and cleanup. Forced termination can leave verified work unfinished.

<a id="execution-proxy-adapter-contract"></a>

## Contract for execution proxy adapters

Generic tests cover admission, correlation, forwarding and ownership, not deployed runtimes. Disable execution until exact runtime version, route inventory and terminal semantics pass qualification. Requalify after relevant runtime, adapter, proxy or ingress changes.

### Route inventory and migration

Record the runtime version and artifact, adapter version, exact methods and paths, classifications and evidence. Apply the [route rules](#route-and-listener-boundary). Matching uses exact method and decoded path.

Query strings, escaped variants, bodies and payloads pass without semantic inspection. Classify by the most privileged possible behavior, including query-triggered enqueue. Check the aliases, trailing slashes, encoding, base paths, redirects and upgrades.

Work-submitting WebSockets are not read-only. Split or gate channels when one HTTP route cannot represent one admission and completion lifecycle. Names and method conventions prove nothing.

### Admission and completion

Preserve the admitted tuple through submission, streaming, tracking and terminal confirmation. Adapters own backend job IDs and interpretation. Follow [completion](#report-completion):

| Runtime behavior | Proof before completion |
| --- | --- |
| Synchronous | Version-specific execution termination, then callback |
| Asynchronous | Correlated terminal state, not acceptance or job ID |
| Streaming | Terminal execution event, not EOF or disconnect |
| Rejection or cancellation | Work cannot still run before `abandoned` |

Execution removes `GetBody`, `Idempotency-Key` and `X-Idempotency-Key` and uses non-rewindable bodies, including empty requests and GET, HEAD, OPTIONS and TRACE. Standard transports never automatically replay ambiguous execution. Payload bytes remain unchanged and known-empty HTTP/1 requests avoid chunked framing. Other routes keep ordinary reuse and transport behavior. This does not deduplicate backend work: never resubmit uncertain execution with a fresh ID.

HTTP 200, HTTP 202 and error responses, EOF, cancellation and transport failure are not completion. Only trusted terminal adapters hold completion authority. Generic upstreams receive tuples in both modes and must not expose them. Authenticate at requester ingress or the deployment gateway and prevent direct bypass.

### Opt-in ComfyUI asynchronous execution

`-comfyui-contract comfyui-a7169322485d0049380fb207fa17e9fb3ec40486` asserts deployment qualification against `Comfy-Org/ComfyUI` commit `a7169322485d0049380fb207fa17e9fb3ec40486`. Default: disabled. It neither detects nor attests installed versions. Check the exact runtime, artifact and custom nodes against [source fixtures](../internal/proxy/testdata/comfyui/README.md), then requalify after changes. The adapter rejects servers that ignore caller `prompt_id`.

Use adapter ingress, separate credentials, a non-native workload and exactly `-execute-route POST:/prompt`. Runtime URL: a root URL on loopback with plain HTTP, without credentials, query, fragment, encoding or base path. Optional monitoring: only `GET:/prompt`, `GET:/queue`, `GET:/history`. No passthrough, job-ID headers, other execution routes, WebSocket submission, query-bearing requests or encoded bodies.

The gateway or requester policy with approved exact-body digest authorizes callers and graphs. Review custom nodes and input and output locations. Envelope validation is not graph safety. Automation receives gateway handles, not raw runtime access or ingress credentials. Isolate every caller and alternate channel against bypass.

For supervisor ownership, generate fresh `prompt_id` and `extra_data.gpu_workload_supervisor.attemptId` UUIDs, replacing caller lifecycle values. One transaction stores admission, original fence, token and UUIDs before dispatch. Keep tuples internal. Raw ComfyUI receives graph data and non-secret IDs only. Store no graphs, inputs, outputs, payloads or artifacts.

One process-lifetime reconciler polls `GET /history/{prompt_id}`. Completion requires exact case-sensitive prompt, correlation and status fields: `status_str: "success"`, `completed: true`, original prompt UUID and attemptId, then absence from both well-formed `/queue` lists. Success proves execution termination, not driver release or artifact validity.

Empty history plus exactly one valid queued or running entry matching both UUIDs is normal pending work without error. Error or cancellation histories and missing or pruned history without correlated queue identity remain unresolved. So do malformed or oversized replies, redirects, unavailable runtime and failed commits. Diagnostics name reasons, never tokens or payloads.

Limits: 16 MiB per envelope and reply. 4096 unfinished attempts. 64 records per pass. 10 seconds per pass and 2 seconds per record. The cursor rotates to prevent starvation.

Shutdown cancels polling before closing state. User forwarding keeps handoff, stop and release gates without supervisor registrations.

Acceptance or disconnect leaves work unfinished. Restart polls stored IDs without replaying `/prompt`. Retained request IDs conflict.

Pre-admission rejection dispatched nothing: get valid admission before retry. After admission, terminal proof or verified `resolve-work` must show earlier execution cannot run before a new execution. Restart or absent queue entries is insufficient.

Submission removes replay hints as above.

Schema v14 adds lifecycle metadata linked to `registered_work`, with cascading deletion under bounded completed-work retention. It preserves registrations, fences, tokens, settings and outcomes. Legacy unfinished work gains no inferred runtime identity.

Older binaries reject v14. Use verified binary and configuration and pre-upgrade [backup rollback](OPERATIONS.md#make-a-backup-restore-and-roll-back-state), never schema edits. Enablement changes neither catalog configuration nor idle policy.

### Failure, restart and ownership

Uncertain start or end stays unfinished through disconnect, truncation, shutdown and restart, preserving correlation. Trusted adapters can later finish with original tuple and proof. Missing tokens or ambiguity need [orphan resolution](OPERATIONS.md#ownership-and-unfinished-work): stop all proxies and runtimes, check release, then record audited abandonment. Ordinary recovery is not terminal proof.

User mode allows selected healthy workloads without lease registration, under the shared handoff gate. Ownership changes close execution, stop user runtimes and await forwarding exit before target start. HTTP enqueue completion is not backend idle. Failed or timed-out handoff keeps admission closed and never restarts stopped user work. Non-execution routes must remain safe during handoff.

### Deployment qualification record

Before enablement, record exact runtime, adapter and proxy versions and route variants, including upgraded channels. Check the terminal semantics and durable correlation without early completion after acceptance or disconnect. Test stale fences, closed admission, duplicate or forged callbacks, closure and drain ordering, client, upstream and proxy failures, restart, orphan resolution and user handoff. Prove bypass prevention for every caller and channel, authentication, callback access, token handling and host and network isolation. Loopback alone proves none of this.

Repository evidence: `internal/proxy/contract_integration_test.go`, `internal/supervisor/proxy_handoff_test.go`, `internal/supervisor/orphan_recovery_test.go`. Real-systemd tests qualify lifecycle and release, not routes or job semantics.

## Orchestrator decision

The state machine, fences, admission and drain remain authoritative. Unit ordering and completion cannot replace readiness, capacity, cgroup or GPU-release evidence.

[#397](https://github.com/mickey-kras/gpu-workload-supervisor/issues/397) adds catalog-derived `Conflicts=` between owned units and `After=` only toward lexically earlier peers. One edge per pair avoids cycles. [systemd](https://www.freedesktop.org/software/systemd/man/latest/systemd.unit.html#Conflicts=) stops conflicts before start in either direction. Keep `Restart=no`.

Add no `StartLimit*`. Manual opposing starts can interrupt work. Arbitrary external units are outside this backstop.

Adopted units and drop-ins remain unchanged.

Optional `owned.conflicts` joins render, spec and fingerprint validation. Absent metadata preserves exact legacy v2 bytes and fingerprints, as optional executable paths do. Unrecognizing readers reject it through strict JSON.

Preview shows replacement writes. Explicit apply upgrades through rollback and recovery journaling. Removing a peer or replacing it with a qualified adopted binding recomputes dependencies.

Guarded files cannot directly qualify for adoption: dependency directives remain outside external grammar and setup never strips them.

Pending activation replays original hashes exactly. Only a later preview after recovery offers upgrades. Startup never rewrites accepted files.

No database migration. Future features get separate schema versions. Catalog limits remain 256 KiB plus 96 KiB for the supported 32-unit graph.

Other fields cannot use that allowance. Full validation and readback allows 352 KiB. Setup requests allow 416 KiB, reserving 64 KiB for profile and envelope.

Keep host evidence and hardware or build identifiers outside the repository. Resource enforcement is not in this release. See the [design](RESOURCE-LIMITS.md).
