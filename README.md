# GPU Workload Supervisor

Crash-safe GPU workload supervision for local AI runtimes.

## Scope

The supervisor owns:

- GPU owner and workload state
- admission control
- lease fencing
- transition journaling
- boot recovery state

It does not own job execution, prompt policy, model APIs, or runtime data.

## State model

New stores start closed and require reconciliation:

```text
owner=supervisor
desiredWorkload=idle
activeWorkload=unknown
phase=reconciling
health=healthy
admission=closed
```

A lease fence combines a store-incarnation UUID with a monotonic epoch. Restore or rebuild rotates the incarnation. A transition rotates the epoch before work is drained.

SQLite uses WAL mode, full synchronous writes, foreign keys, and one database connection. Transition side effects are recorded as intent and observation events.

## Development

```sh
go test ./...
go vet ./...
```

## Local CLI

Runtime identity is deployment configuration. Unit names, endpoints, and a measured post-release GPU-memory threshold are required:

```sh
gpu-mode \
  -text-unit TEXT.service \
  -media-unit MEDIA.service \
  -text-health-url http://127.0.0.1:PORT/health \
  -media-health-url http://127.0.0.1:PORT/ \
  -media-release-url http://127.0.0.1:PORT/free \
  status|reconcile|recover|text|media|idle
```

Defaults:

- state: `$XDG_STATE_HOME/gpu-workload-supervisor/state.db` or `~/.local/state/gpu-workload-supervisor/state.db`
- health request timeout: 10 seconds
- drain timeout: 5 minutes
- readiness timeout: 5 minutes
- rollback timeout: 2 minutes
- failure finalization timeout: 10 seconds
- poll interval: 250 milliseconds

The state directory must be private and owned by the current user. Commands use an exclusive file lock. Opening the store applies pending migrations. `status` persists a closed error state if runtime observation violates the single-GPU invariant.

ComfyUI process availability is separate from media GPU ownership. Entering media mode stops text inference and keeps ComfyUI available. Leaving media mode calls the configured model-release endpoint, then polls the NVML-backed `nvidia-smi` memory reading below the deployment threshold before text inference starts.

Boot reconciliation never resumes media work. Interrupted transitions and latched errors require explicit `recover`.

Remote control, authorization, UI, Job Broker implementation, and host-specific deployment remain outside this repository slice.
