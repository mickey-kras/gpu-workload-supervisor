# Demand activation and inactivity idle plan

Plan for [issue #144](https://github.com/mickey-kras/gpu-workload-supervisor/issues/144). No code, no settings, no timers. Merging enables nothing.

## Placement

- One policy loop in the supervisor backend, next to the transition controller. Never in GNOME Shell or any client.
- Reads durable evidence only; drives existing typed controller operations. No second lifecycle engine, no new authority, no runtime-specific logic.
- Desktop UI displays committed policy + verified pending-idle deadline from backend state. UI disabled or session locked: no behavior change.

## Blockers before implementation

1. Orchestration adapters per #59/#60 deployed and qualified. Merged contracts are not adapters.
2. Reliable queued/reserved/running/unresolved-work evidence; direct execution bypass prevented at the deployment boundary.
3. Typed operator settings interface (#61 follow-up) before any policy value reaches the UI.
4. Per-workload activity evidence: admission + verified terminal completion (native bindings: #212, #269).

## Activation

- Authorized request + Supervisor ownership + Idle -> switch, verify readiness, open admission.
- Mutual exclusion preserved; waits for other work, never interrupts. Queue/job state stays with orchestration.
- User ownership, transition in progress, or recovery latch -> reject. Nothing activates implicitly.

## Idle transition

Both required, committed as one fenced decision (state version + lease fence):

1. Inactivity period elapsed since last recorded workload activity.
2. No queued, reserved, running or unresolved work, including admitted registrations without terminal evidence.

Admission between check and switch aborts the transition; no shutdown race.

## Activity definition

- Counts: admission registrations, verified terminal completion.
- Never counts: UI polling, health checks, keyboard/session input, GPU utilization, VRAM level.
- Loaded-but-unused = inactivity. Disconnect without terminal evidence = unresolved work.

## Settings

- One setting: timeout minutes + Off. Proposed 60. Enabled default only after host measurement below.
- Persisted via typed operator settings. On restart, pending deadline is recomputed from durable last-activity, never a process-local timer.
- Uncertain or missing evidence -> no idle transition.

## Boundaries

- Never reclaim User ownership; never idle User-mode work.
- No automatic recovery from a latched error; a latch blocks all policy transitions.
- No firmware, clock, power-limit or system-suspend changes.
- Host identity, runtime routes, measurements, installation: private deployment configuration.

## Acceptance

- Cases: request at exact timeout, cold activation, mixed workloads, streaming and async completion, long jobs crossing timeout, disconnects, ambiguous completion, restart with pending deadline, ownership transfer, rejected requests, recovery latches.
- Measure loaded-but-unused vs idle power and activation latency on the deployment host; record in the private qualification record before any savings claim or enabled default.
