# Demand activation and inactivity idle plan

Plan for [issue #144](https://github.com/mickey-kras/gpu-workload-supervisor/issues/144). No code, no settings, no timers. Merging enables nothing.

## Placement

- One policy loop in the supervisor backend, next to the transition controller. Never in GNOME Shell or any client.
- Reads durable evidence only; drives existing typed controller operations. No second lifecycle engine, no new authority, no runtime-specific logic.
- Desktop UI displays committed policy + verified pending-idle deadline from backend state. UI disabled or session locked: no behavior change.

## Blockers before implementation

1. Orchestration adapters per #59/#60 deployed and qualified. Merged contracts are not adapters.
2. Reliable queued/reserved/running/unresolved-work evidence; direct execution bypass prevented at the deployment boundary.
3. Typed local operator settings surface created and tracked as a prerequisite issue. No typed-settings issue exists today; the #61 follow-up reference is stale. No policy value reaches the UI before this surface lands.
4. Per-workload activity evidence: admission + verified terminal completion (native bindings: #212; #269 is merged at fixture level, host qualification remains outstanding).
5. Activation ingress decision (below) resolved and implemented.

## Activation ingress: open design decision

"Authorized request activates workload" has no carrier today. The externalcontrol contract was deleted (#235); the operator protocol has no workload-request action (only status, take-control, user-switch, return-control); proxies are deployment-launched and reject registrations while admission is closed. Decision required before any activation work:

- Option A: new typed operator action carrying a workload request.
- Option B: revived external facade accepting requests ahead of activation.

Constraints either option must satisfy:

1. Typed local operator authority only; no remote control interface, per #212/#146 rules.
2. After admission reopens, the lease fence is distributed to adapters; stale-fence registrations stay rejected.
3. Requests remain orchestration-queued until activation completes; the supervisor never holds queue state.
4. Rejection path when User ownership, transition in progress, or recovery latch applies.

Do not implement either option under this plan; record the decision and its contract first.

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
2. Push vs pull: open decision. Pull on each policy evaluation is the default; push invalidation may supplement but never replaces a fresh pull at decision time.
3. Freshness: the attestation carries an adapter timestamp; attestations older than a bounded staleness limit are treated as missing. Limit value set at implementation, recorded here. **Implementation: 120 seconds (`MaxAttestationAge` in `internal/supervisor/policy.go`); the user timer ticks every 60 seconds (`packaging/gpu-workload-supervisor-idle.timer`).**
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
- Persisted via the typed local operator settings surface (prerequisite, see Blockers). On restart, pending deadline is recomputed from durable last-activity, never a process-local timer.
- Uncertain or missing evidence -> no idle transition.

## Status surface work item

- `control.State` and `operator.Status` carry no policy or deadline fields today.
- Add exactly two fields: committed policy (timeout minutes + Off) and verified pending-idle deadline.
- No savings estimate, no live countdown; countdown is UI-side rendering of the deadline only.

## Boundaries

- Never reclaim User ownership; never idle User-mode work.
- No automatic recovery from a latched error; a latch blocks all policy transitions.
- No firmware, clock, power-limit or system-suspend changes.
- Host identity, runtime routes, measurements, installation: private deployment configuration.

## Acceptance

- Cases: request at exact timeout, cold activation, activation during active work, mixed workloads, streaming and async completion, long jobs crossing timeout, disconnects, ambiguous completion, adapter outage and stale attestation, restart with pending deadline, ownership transfer with armed deadline, rejected requests, recovery latches.
- Measure loaded-but-unused vs idle power and activation latency on the deployment host; record in the private qualification record before any savings claim or enabled default.
