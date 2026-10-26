# Demand activation and inactivity idle plan

Implementation and remaining qualification for [issue
#144](https://github.com/mickey-kras/gpu-workload-supervisor/issues/144). Durable
policy, typed settings and demand activation and a 60-second user timer exist. Policy
defaults to Off.

Packaged entrypoints have no qualified evidence provider: enabling returns
`unavailable`, and policy evaluation fails closed. Keep Off for initial desktop
deployment.

## Placement

- One policy loop in the supervisor backend, next to the transition controller. Never in GNOME Shell or any client.
- Reads durable evidence only. Drives existing typed controller operations. No second lifecycle engine, no new authority, no runtime-specific logic.
- Desktop integration must display committed policy and only a verified pending-idle deadline from backend state. UI disabled or session locked: no behavior change.

## Required before enablement

1. Deploy and qualify an orchestration `EvidenceProvider`. Contracts alone are not adapters.
2. Check the evidence for queued, reserved, running and unresolved work and prevent direct execution bypass at the deployment boundary.
3. Qualify native terminal completion and model lifecycle on the deployment host. Fixture coverage is not host acceptance.
4. Integrate the desktop presentation of settings and deadlines only when backend capability and fresh evidence support it.

[REQUESTER-INGRESS.md](REQUESTER-INGRESS.md) describes the optional authenticated requester and workflow
ingress and the durable queued and reserved demand. Production
isolation qualification remains outstanding. Auto-idle stays Off.

## Activation ingress

The typed local action `activate-workload` activates workloads on demand. See
[OPERATOR.md](OPERATOR.md). It requires a fresh state precondition and returns the new
lease fence only after the controller verifies readiness and admission. There is no
remote control facade.

- Queue and job state stays with orchestration until activation completes.
- Adapters receive the fresh fence. Stale-fence registrations remain rejected.
- User ownership, busy transitions and recovery latches reject activation.

## Activation

- An authorized request under Supervisor ownership in Idle starts a switch. The controller checks readiness and opens admission.
- Mutual exclusion preserved. Waits for other work, never interrupts. Queue and job state stays with orchestration.
- Activation requested during active work: the request waits with orchestration. The supervisor returns a typed busy or deferred response. No interrupt, no preemption.
- The controller rejects activation during User ownership, a transition in progress, or a recovery latch. Nothing activates implicitly.

## Idle transition

Both required, committed as one fenced decision (state version and lease fence):

1. Inactivity period elapsed since last recorded workload activity.
2. No queued, reserved, running or unresolved work, including admitted registrations without terminal evidence.

Admission between check and switch aborts the transition. No shutdown race.

## Evidence input contract

The policy loop consumes one attestation from the orchestration adapter. No other
evidence source.

1. Content: queued, reserved, running, and unresolved work sets, each as a set of job identities. Unresolved maps to store primitives: `PendingWork` (registrations retaining completion authority). `ResolveUnfinishedWork` stays reserved for verified operator recovery, never for policy cleanup.
2. Pull `Attest` on each evaluation. `AcquireFence` must validate its generation and exclude queue or reservation changes through the idle transition transaction. Invalidation cannot replace fresh evidence.
3. Freshness: the attestation carries an adapter timestamp. The policy treats attestations older than a bounded staleness limit as missing. Implementation: 120 seconds (`MaxAttestationAge` in `internal/supervisor/policy.go`). The user timer ticks every 60 seconds (`packaging/gpu-workload-supervisor-idle.timer`).
4. Adapter outage or unreachable adapter -> fail closed: evidence counts as missing, no idle transition.
5. Uncertain, partial, or missing attestation -> no idle transition.

<a id="ownership-transfer-while-pending-idle-is-armed"></a>

## Ownership transfer with an armed idle deadline

- Any ownership transfer (take-control by User, or return-control completing) cancels and disarms a pending idle deadline.
- A disarmed deadline recomputes only from fresh post-transfer activity evidence.
- Never idle User-mode work. The policy loop exits while Owner is User.

## Activity definition

- Counts: admission registrations, verified terminal completion.
- Never counts: UI polling, health checks, keyboard or session input, GPU use, VRAM level.
- Loaded-but-unused = inactivity. A disconnect without terminal evidence means unresolved work.

## Settings

- One setting: timeout minutes + Off. Proposed 60. Enabled default only after host measurement below.
- Persisted through `get-settings`/`set-idle-policy`, with state and settings revision checks. On restart, the policy recomputes the pending deadline from durable last-activity. It never uses a process-local timer.
- Uncertain or missing evidence -> no idle transition.

<a id="status-surface-work-item"></a>

## Status interface

- `control.State` carries policy and deadline read-model fields. Committed operator settings are available through `get-settings`. Ordinary operator `status` is not a settings snapshot.
- Desktop policy and deadline presentation remains a follow-up. Expose a pending deadline only from verified backend evidence.
- No savings estimate, no live countdown. Countdown is UI-side rendering of the deadline only.

## Boundaries

- Never reclaim User ownership. Never idle User-mode work.
- No automatic recovery from a latched error. A latch blocks all policy transitions.
- No firmware, clock, power-limit or system-suspend changes.
- Host identity, runtime routes, measurements, installation: private deployment configuration.

## Acceptance

- Cases: request at exact timeout, cold activation, activation during active work, mixed workloads, streaming and async completion.
- Also test long jobs crossing timeout, disconnects, ambiguous completion, adapter outage and stale attestation.
- Also test restart with pending deadline, ownership transfer with armed deadline, rejected requests and recovery latches.
- Measure the power for loaded-but-unused and idle states and activation latency on the deployment host. Record in the private qualification record before any savings claim or enabled default.
