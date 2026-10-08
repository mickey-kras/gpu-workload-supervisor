# Demand activation and inactivity idle plan

Implementation and remaining qualification for [issue #144](https://github.com/mickey-kras/gpu-workload-supervisor/issues/144). Durable policy, typed settings/demand activation and a 60-second user timer exist. Policy defaults to Off. Packaged entrypoints have no qualified evidence provider: enabling returns `unavailable`, and policy evaluation fails closed. Keep Off for initial desktop deployment.

## Placement

- One policy loop in the supervisor backend, next to the transition controller. Never in GNOME Shell or any client.
- Reads durable evidence only; drives existing typed controller operations. No second lifecycle engine, no new authority, no runtime-specific logic.
- Desktop integration must display committed policy and only a verified pending-idle deadline from backend state. UI disabled or session locked: no behavior change.

## Required before enablement

1. Deploy and qualify an orchestration `EvidenceProvider`; contracts alone are not adapters.
2. Verify queued/reserved/running/unresolved evidence and prevent direct execution bypass at the deployment boundary.
3. Qualify native terminal completion and model lifecycle on the deployment host; fixture coverage is not host acceptance.
4. Integrate desktop settings/deadline presentation only when backend capability and fresh evidence support it.

## Activation ingress

The typed local `activate-workload` action implements demand activation; see
[OPERATOR.md](OPERATOR.md). It requires a fresh state precondition and returns the
new lease fence only after readiness and admission are verified. There is no
remote control facade.

- Queue/job state stays with orchestration until activation completes.
- Adapters receive the fresh fence; stale-fence registrations remain rejected.
- User ownership, busy transitions and recovery latches reject activation.

## Activation

- Authorized request + Supervisor ownership + Idle -> switch, verify readiness, open admission.
- Mutual exclusion preserved; waits for other work, never interrupts. Queue/job state stays with orchestration.
- Activation requested during active work: the request waits with orchestration; the supervisor returns a typed busy/deferred response; no interrupt, no preemption.
- User ownership, transition in progress, or recovery latch -> reject. Nothing activates implicitly.

## Idle transition

Both required, committed as one fenced decision (state version + lease fence):

1. Inactivity period elapsed since last recorded workload activity.
2. No queued, reserved, running or unresolved work, including admitted registrations without terminal evidence.

Admission between check and switch aborts the transition; no shutdown race.

## Evidence input contract

The policy loop consumes one attestation from the orchestration adapter; no other evidence source.

1. Content: queued, reserved, running, and unresolved work sets, each as a set of job identities. Unresolved maps to store primitives: `PendingWork` (registrations retaining completion authority); `ResolveUnfinishedWork` stays reserved for verified operator recovery, never for policy cleanup.
2. Pull `Attest` on each evaluation. `AcquireFence` must validate its generation and exclude queue/reservation changes through the idle transition transaction; invalidation cannot replace fresh evidence.
3. Freshness: the attestation carries an adapter timestamp; attestations older than a bounded staleness limit are treated as missing. **Implementation: 120 seconds (`MaxAttestationAge` in `internal/supervisor/policy.go`); the user timer ticks every 60 seconds (`packaging/gpu-workload-supervisor-idle.timer`).**
4. Adapter outage or unreachable adapter -> fail closed: evidence counts as missing, no idle transition.
5. Uncertain, partial, or missing attestation -> no idle transition.

## Ownership transfer while pending idle is armed

- Any ownership transfer (take-control by User, or return-control completing) cancels and disarms a pending idle deadline.
- A disarmed deadline recomputes only from fresh post-transfer activity evidence.
- Never idle User-mode work; the policy loop exits while Owner is User.

## Activity definition

- Counts: admission registrations, verified terminal completion.
- Never counts: UI polling, health checks, keyboard/session input, GPU utilization, VRAM level.
- Loaded-but-unused = inactivity. Disconnect without terminal evidence = unresolved work.

## Settings

- One setting: timeout minutes + Off. Proposed 60. Enabled default only after host measurement below.
- Persisted through `get-settings`/`set-idle-policy`, with state and settings revision checks. On restart, pending deadline is recomputed from durable last-activity, never a process-local timer.
- Uncertain or missing evidence -> no idle transition.

## Status surface work item

- `control.State` carries policy/deadline read-model fields; committed operator settings are available through `get-settings`. Ordinary operator `status` is not a settings snapshot.
- Desktop policy/deadline presentation remains a follow-up. Expose a pending deadline only from verified backend evidence.
- No savings estimate, no live countdown; countdown is UI-side rendering of the deadline only.

## Boundaries

- Never reclaim User ownership; never idle User-mode work.
- No automatic recovery from a latched error; a latch blocks all policy transitions.
- No firmware, clock, power-limit or system-suspend changes.
- Host identity, runtime routes, measurements, installation: private deployment configuration.

## Acceptance

- Cases: request at exact timeout, cold activation, activation during active work, mixed workloads, streaming and async completion, long jobs crossing timeout, disconnects, ambiguous completion, adapter outage and stale attestation, restart with pending deadline, ownership transfer with armed deadline, rejected requests, recovery latches.
- Measure loaded-but-unused vs idle power and activation latency on the deployment host; record in the private qualification record before any savings claim or enabled default.
