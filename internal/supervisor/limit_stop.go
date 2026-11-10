package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

var (
	// ErrLimitStopDeferred marks a run of EnforceLimits that stopped nothing
	// and that the next tick retries: admitted work of the unit is pending
	// before its deadline, or a check or read proved nothing. The tick
	// writes it to the log and exits zero.
	ErrLimitStopDeferred = errors.New("the limit stop is deferred to the next tick")
	// ErrLimitCheckSkipped marks a run with no stored failure whose step
	// before the limit checks failed. No limit check ran, so no stop is
	// pending. The tick writes it to the log and exits zero.
	ErrLimitCheckSkipped = errors.New("limit check skipped")
	// ErrLimitStopOverdue marks a deferral that lasted longer than the drain
	// and verify timeouts after the first failed check. The stop stays
	// deferred, but the tick exits nonzero.
	ErrLimitStopOverdue = errors.New("the limit stop is overdue")
	// ErrLimitStopped marks a run whose stop through the system manager ran,
	// also when the release check after it failed. It also marks each later
	// run while a committed stop holds the latch. The tick exits nonzero
	// until recover, so failed-unit monitoring sees the stop.
	ErrLimitStopped = errors.New("a unit with resource limits was stopped; the error state stays until recover")
	// ErrLimitStopFailed marks a limit stop that is not finished: the stop
	// did not run, its record write failed, the release check still fails,
	// or a started stop cannot go on. The tick exits nonzero until recover.
	// When a later tick finishes the stop, the class becomes
	// ErrLimitStopped.
	ErrLimitStopFailed = errors.New("the limit stop did not finish")
)

// limitStopInitiator names the journaled transition of a limit stop.
const limitStopInitiator = "resource-limit-stop"

// limitStopRuntime gives the outcome of the limits of a unit before its
// stop, and the release check after it.
type limitStopRuntime interface {
	LimitOutcome(context.Context, control.Workload) gpuruntime.LimitOutcome
	LimitStopReleased(context.Context, control.Workload) error
}

// TickWithLimits runs the limit stop and then the inactivity policy. It is
// the policy tick of gpu-mode and of gpu-setup.
func (c *Controller) TickWithLimits(ctx context.Context, evidence EvidenceProvider, log io.Writer) error {
	return errors.Join(c.LimitTick(ctx, log), c.PolicyTick(ctx, evidence))
}

// LimitTick runs EnforceLimits for the policy tick. A deferred stop goes to
// log and is not an error, so the tick exits zero and retries. An overdue
// deferral, a stop or a failed stop returns its cause: the tick exits
// nonzero, and the service journal records why.
func (c *Controller) LimitTick(ctx context.Context, log io.Writer) error {
	err := c.EnforceLimits(ctx)
	if errors.Is(err, ErrLimitStopDeferred) || errors.Is(err, ErrLimitCheckSkipped) {
		_, _ = fmt.Fprintln(log, err)
		return nil
	}
	return err
}

// EnforceLimits stops a running unit with limits whose checks fail, while
// the supervisor-owned state is latched with admission closed. Status
// latches on a failed check but never stops a workload. This method acts
// only on a latched state, so a healthy state gets no new checks.
//
// The store keeps when the first failure of the latch came and when the
// first definite failure came. With admitted work of the unit pending, the
// stop waits until DrainTimeout after the first definite failure, or until
// the overdue time when that comes first, then cuts the work off. A failure that proves nothing waits until DrainTimeout plus
// VerifyTimeout after the first failure, then the unit stops too. The stop
// is a journaled transition through the system manager. A passed release
// check commits idle; otherwise the workload is unknown. The error state
// stays in both cases. Once a stop started, each tick exits nonzero until
// recover ends the latch.
func (c *Controller) EnforceLimits(ctx context.Context) error {
	stop, stored, recordErr := c.store.LimitStop(ctx)
	state, err := c.store.State(ctx)
	if err != nil {
		if stored {
			return c.recordedStepFailure(stop, control.State{}, fmt.Errorf("state read: %w", err))
		}
		return fmt.Errorf("limit stop: state read: %w", errors.Join(recordErr, err))
	}
	latched := limitLatched(state)
	started := stored && !stop.StoppingAt.IsZero()
	switch {
	case recordErr != nil && latched:
		// The record can hold a started stop, so the tick fails.
		return fmt.Errorf("%w: limit stop record: %w", ErrLimitStopFailed, recordErr)
	case recordErr != nil:
		return deferLimitStop("limit stop record", recordErr)
	case latched && started && !c.config.Catalog.Catalog.HasLimits():
		return c.recordedStepFailure(stop, state, errors.New("the catalog has no unit with limits now"))
	case !latched || !c.config.Catalog.Catalog.HasLimits():
		// A record from an earlier latch or catalog no longer applies.
		return c.clearStaleLimitStop(ctx, stored)
	}
	// A failed step before the observation escalates only from a stored
	// failure: a latch for another reason is no limit problem.
	step := func(name string, err error) error {
		if stored {
			return c.recordedStepFailure(stop, state, fmt.Errorf("%s: %w", name, err))
		}
		return fmt.Errorf("%w: %s: %w", ErrLimitCheckSkipped, name, err)
	}
	if err := c.noTransitionRuns(ctx); err != nil {
		return step("transition check", err)
	}
	if err := c.checkCatalog(ctx); err != nil {
		return step("catalog check", err)
	}
	snapshot, peerErr, err := c.observeLimitCandidates(ctx)
	if err != nil {
		return step("runtime observation", err)
	}
	return withFailedPeer(c.enforceObserved(ctx, state, snapshot, stop, stored), peerErr)
}

// enforceObserved acts on the observation of the running units.
func (c *Controller) enforceObserved(ctx context.Context, state control.State, snapshot gpuruntime.Snapshot, stop store.LimitStop, stored bool) error {
	if stored && !stop.StoppingAt.IsZero() {
		// A started stop only goes on to its end: no other check, record
		// or deferral can replace it.
		return c.settleStartedStop(ctx, state, snapshot, stop)
	}
	if failure := c.firstLimitFailure(ctx, snapshot); failure.profile.ID != "" {
		return c.enforceUnitLimits(ctx, state, failure)
	}
	return c.settleLimitRecord(ctx, snapshot, stop, stored)
}

// observeLimitCandidates observes the units that the limit checks need.
// The strict observation rejects a failed unit, for example after an OOM
// kill. Then the recovery observation, which reports a failed unit as not
// running, still finds the running units with limits, and peerErr keeps
// the strict failure. Neither observation is release evidence: the release
// check after a stop stays strict and fails while any unit has failed.
func (c *Controller) observeLimitCandidates(ctx context.Context) (snapshot gpuruntime.Snapshot, peerErr, err error) {
	snapshot, err = c.observe(ctx)
	if err == nil {
		return snapshot, nil, nil
	}
	recovered, recoveryErr := c.observeForReconciliation(ctx, true)
	if recoveryErr != nil {
		return recovered, nil, err
	}
	return recovered, err, nil
}

// withFailedPeer keeps the tick nonzero when the strict observation
// failed: a failed unit is an error that the operator must see. The limit
// checks of the running units still ran, and their result stays in the
// text.
func withFailedPeer(result, peerErr error) error {
	if peerErr == nil {
		return result
	}
	if result != nil && !errors.Is(result, ErrLimitStopDeferred) && !errors.Is(result, ErrLimitCheckSkipped) {
		return errors.Join(result, fmt.Errorf("runtime observation: %w", peerErr))
	}
	text := "the limit checks of the running units found no stop"
	if result != nil {
		text = result.Error()
	}
	return fmt.Errorf("runtime observation: %w; %s", peerErr, text)
}

// recordedStepFailure handles a failed step with a stored record. Before
// the stop starts, it defers or escalates. Once a stop started, the tick
// exits nonzero at once: a committed stop keeps ErrLimitStopped, and any
// other started stop, such as one that a crash interrupted, is not
// finished. An unreadable state gives ErrLimitStopFailed.
func (c *Controller) recordedStepFailure(stop store.LimitStop, state control.State, cause error) error {
	if stop.StoppingAt.IsZero() {
		return c.deferOrEscalate(stop.Workload, stop.FirstSeenAt, cause)
	}
	class := ErrLimitStopFailed
	if state.ActiveWorkload == control.WorkloadIdle {
		class = ErrLimitStopped
	}
	return fmt.Errorf("%w: the limit stop of %s started; run recover: %w", class, stop.Workload, cause)
}

// limitLatched reports a supervisor-owned state with error health and
// closed admission.
func limitLatched(state control.State) bool {
	return state.Owner == control.OwnerSupervisor && state.Health == control.HealthError && state.Admission == control.AdmissionClosed
}

// noTransitionRuns fails while a transition is in progress. A limit stop
// interrupted by a crash leaves its transition in progress, and recover
// ends it.
func (c *Controller) noTransitionRuns(ctx context.Context) error {
	running, err := c.store.InProgressTransition(ctx)
	if err != nil {
		return err
	}
	if running != "" {
		return fmt.Errorf("%w: %s", ErrTransitionRunning, running)
	}
	return nil
}

// limitFailure is the failed check of one running unit with limits.
type limitFailure struct {
	profile control.WorkloadProfile
	err     error
}

// definite reports a check that proves a limit or launch binding wrong.
func (f limitFailure) definite() bool {
	return gpuruntime.DefiniteLimitFailure(f.err) || gpuruntime.DefiniteBindingFailure(f.err)
}

// firstLimitFailure checks every running unit with limits. A definite
// failure comes before any failure that proves nothing.
func (c *Controller) firstLimitFailure(ctx context.Context, snapshot gpuruntime.Snapshot) limitFailure {
	var deferred limitFailure
	for _, p := range c.config.Catalog.Catalog.Profiles {
		if !p.HasLimits() || !snapshot.Workloads[p.ID].Active {
			continue
		}
		f := limitFailure{profile: p, err: c.checkRetainedLimits(ctx, p)}
		if f.err == nil {
			continue
		}
		if f.definite() {
			return f
		}
		if deferred.profile.ID == "" {
			deferred = f
		}
	}
	return deferred
}

// clearStaleLimitStop removes a stored limit stop that no running unit
// needs.
func (c *Controller) clearStaleLimitStop(ctx context.Context, stored bool) error {
	if !stored {
		return nil
	}
	if err := c.store.ClearLimitStop(ctx); err != nil {
		return fmt.Errorf("limit stop: remove the stale record: %w", err)
	}
	return nil
}

func deferLimitStop(step string, err error) error {
	return fmt.Errorf("%w: %s: %w", ErrLimitStopDeferred, step, err)
}

// overdue reports that more than DrainTimeout plus VerifyTimeout passed
// since since.
func (c *Controller) overdue(since time.Time) bool {
	return c.now().After(since.Add(c.config.DrainTimeout + c.config.VerifyTimeout))
}

// deferOrEscalate defers the stop of workload. Once the deferral is
// overdue, the stop stays deferred, but the error is no longer a deferral,
// so the tick exits nonzero and failed-unit monitoring sees it.
func (c *Controller) deferOrEscalate(workload control.Workload, since time.Time, cause error) error {
	if c.overdue(since) {
		return fmt.Errorf("%w: %s since %s: %w", ErrLimitStopOverdue, workload, since.UTC().Format(time.RFC3339), cause)
	}
	return fmt.Errorf("%w: %w", ErrLimitStopDeferred, cause)
}

// limitStopPlan says why a unit stops and until when its work may drain.
// A zero drainEnd cuts pending work off at once.
type limitStopPlan struct {
	reason   string
	drainEnd time.Time
	cause    error
}

// enforceUnitLimits records the failed check of one running unit and then
// defers or stops it. Once the record has a definite time, the definite
// plan applies, whatever a later failed check shows. A passing check
// holds the stop: settleLimitRecord keeps the record and its times. Its drain ends at the
// earlier of DefiniteAt plus DrainTimeout and the overdue time, so a
// failure that turns definite late never stops later than one that stays
// unproven. A failure that proves nothing stops the unit once it is
// overdue, and its admitted work is then cut off at once. A failed record
// write is no deferral: the tick exits nonzero.
func (c *Controller) enforceUnitLimits(ctx context.Context, state control.State, f limitFailure) error {
	p := f.profile
	stop, err := c.store.MarkLimitFailure(ctx, p.ID, f.definite(), c.now())
	if err != nil {
		return fmt.Errorf("%w: %w; limit stop record of %s: %w", ErrLimitStopFailed, limitedUnitError(ErrLimitedUnitRejected, p, f.err), p.ID, err)
	}
	if !stop.DefiniteAt.IsZero() {
		reason, cause := "definite check failure", limitedUnitError(ErrLimitedUnitRejected, p, f.err)
		if !f.definite() {
			reason, cause = "the latch has a recorded definite failure; the check of "+string(p.ID)+" proves nothing", gpuruntime.UnitError(p, f.err)
		}
		drainEnd := stop.DefiniteAt.Add(c.config.DrainTimeout)
		if overdueAt := stop.FirstSeenAt.Add(c.config.DrainTimeout + c.config.VerifyTimeout); overdueAt.Before(drainEnd) {
			drainEnd = overdueAt
		}
		return c.stopLimitedUnit(ctx, state, stop, p, limitStopPlan{reason: reason, drainEnd: drainEnd, cause: cause})
	}
	if !c.overdue(stop.FirstSeenAt) {
		// Nothing is proven yet; the closed admission stays.
		return fmt.Errorf("%w: %w", ErrLimitStopDeferred, gpuruntime.UnitError(p, f.err))
	}
	return c.stopLimitedUnit(ctx, state, stop, p, limitStopPlan{
		reason: "overdue check failure that proves nothing", cause: gpuruntime.UnitError(p, f.err)})
}

// stopLimitedUnit stops the unit when no admitted work of it is pending,
// or once the drain ends. It records the start of the stop before the
// stop, and it reads the outcome of the limits while the cgroup exists.
// Only a stop that ran gives ErrLimitStopped; it also reports cut-off work
// when the release check after it fails.
func (c *Controller) stopLimitedUnit(ctx context.Context, state control.State, stop store.LimitStop, p control.WorkloadProfile, plan limitStopPlan) error {
	pending, err := c.store.PendingWorkFor(ctx, p.ID)
	if err != nil {
		return c.deferOrEscalate(p.ID, stop.FirstSeenAt, fmt.Errorf("%w; pending work of %s: %w", plan.cause, p.ID, err))
	}
	if pending > 0 && c.now().Before(plan.drainEnd) {
		return c.deferOrEscalate(p.ID, stop.FirstSeenAt,
			fmt.Errorf("%w; admitted work of %s is pending; complete it, or run resolve-work to abandon it", plan.cause, p.ID))
	}
	outcome := c.limitOutcome(ctx, p.ID)
	if err := c.store.MarkLimitStopping(ctx, p.ID, c.now()); err != nil {
		return fmt.Errorf("%w: %w; record the limit stop of %s: %w", ErrLimitStopFailed, plan.cause, p.ID, err)
	}
	stopped, result := c.journaledLimitStop(ctx, state, p, plan.reason+"; limit outcome: "+outcome.String())
	if !stopped {
		return errors.Join(fmt.Errorf("%w: %s: %s: %w; limit outcome: %s: %w", ErrLimitStopFailed, p.ID, plan.reason, plan.cause, outcome, result),
			cutOffWork(p.ID, pending, nil, workNotStopped))
	}
	how := workCutOff
	if !plan.drainEnd.IsZero() {
		how = workDrained
	}
	return errors.Join(fmt.Errorf("%w: %s: %w; limit outcome: %s", ErrLimitStopped, plan.reason, plan.cause, outcome),
		cutOffWork(p.ID, pending, nil, how), result)
}

// How a stop treated pending admitted work.
const (
	workCutOff = iota
	workDrained
	workNotStopped
)

// cutOffWork reports admitted work of a stop: cut off, cut off after a
// drain, or still pending because the stop did not run. Each text tells
// the operator to run resolve-work.
func cutOffWork(id control.Workload, pending int, readErr error, how int) error {
	switch {
	case readErr != nil:
		return fmt.Errorf("pending work of %s is unknown; check it, and run resolve-work for cut-off work: %w", id, readErr)
	case pending == 0:
		return nil
	case how == workDrained:
		return fmt.Errorf("admitted work of %s did not drain before the deadline and was cut off; run resolve-work for it", id)
	case how == workNotStopped:
		return fmt.Errorf("admitted work of %s is pending, and the next stop cuts it off; run resolve-work for it", id)
	}
	return fmt.Errorf("admitted work of %s was cut off; run resolve-work for it", id)
}

// limitOutcome reads the outcome of the limits of a unit as text. It is
// diagnostics only: it decides nothing.
func (c *Controller) limitOutcome(ctx context.Context, id control.Workload) gpuruntime.LimitOutcome {
	probeCtx, cancel := context.WithTimeout(ctx, c.config.ActionTimeout)
	defer cancel()
	return c.runtime.LimitOutcome(probeCtx, id)
}

// settleLimitRecord handles a record with no started stop when no running
// unit fails. A unit that still runs keeps its first failure times, so one
// passing check does not reset them. A unit that stopped by itself, or
// that left the catalog, needs no record.
func (c *Controller) settleLimitRecord(ctx context.Context, snapshot gpuruntime.Snapshot, stop store.LimitStop, stored bool) error {
	if !stored || snapshot.Workloads[stop.Workload].Active {
		return nil
	}
	if err := c.store.ClearLimitStop(ctx); err != nil {
		return fmt.Errorf("limit stop: remove the settled record: %w", err)
	}
	return nil
}

// settleStartedStop takes a started stop to its end. A unit that still
// runs is stopped again, whatever its checks show now. A stopped unit gets
// its release check, and a released Idle state holds the latch. Every result
// exits nonzero until recover ends the latch.
func (c *Controller) settleStartedStop(ctx context.Context, state control.State, snapshot gpuruntime.Snapshot, stop store.LimitStop) error {
	p, ok := c.config.Catalog.Catalog.Profile(stop.Workload)
	if !ok {
		return fmt.Errorf("%w: the unit of the started limit stop of %s left the catalog; run recover", ErrLimitStopFailed, stop.Workload)
	}
	if snapshot.Workloads[p.ID].Active {
		return c.retryStartedStop(ctx, state, p)
	}
	return c.finishEarlierStop(ctx, state, p)
}

// retryStartedStop runs a started stop again for a unit that still runs.
// Admitted work of the unit gets no new drain: the stop cuts it off.
func (c *Controller) retryStartedStop(ctx context.Context, state control.State, p control.WorkloadProfile) error {
	outcome := c.limitOutcome(ctx, p.ID)
	pending, pendingErr := c.store.PendingWorkFor(ctx, p.ID)
	stopped, err := c.journaledLimitStop(ctx, state, p, "retry an earlier limit stop; limit outcome: "+outcome.String())
	if !stopped {
		return errors.Join(fmt.Errorf("%w: the earlier limit stop of %s; limit outcome: %s: %w", ErrLimitStopFailed, p.ID, outcome, err),
			cutOffWork(p.ID, pending, pendingErr, workNotStopped))
	}
	return errors.Join(fmt.Errorf("%w: retried the earlier limit stop of %s; limit outcome: %s", ErrLimitStopped, p.ID, outcome),
		cutOffWork(p.ID, pending, pendingErr, workCutOff), err)
}

// finishEarlierStop finishes a stop whose unit no longer runs. It probes
// the release once with no poll and no transition. A failed probe exits
// nonzero. With a passed probe, an Idle state holds the latch: the stop
// is done and released. Otherwise the stop runs again as a journaled
// transition, which records idle when its release check passes. Idle
// alone is no proof: the probe comes first.
func (c *Controller) finishEarlierStop(ctx context.Context, state control.State, p control.WorkloadProfile) error {
	// The unit no longer runs, so its pending work is cut off.
	pending, pendingErr := c.store.PendingWorkFor(ctx, p.ID)
	work := cutOffWork(p.ID, pending, pendingErr, workCutOff)
	if err := c.probeLimitStopRelease(ctx, p.ID); err != nil {
		remedy := "the next tick checks again"
		if gpuruntime.DefiniteBindingFailure(err) {
			remedy = "the launch binding changed; restore it, then run recover"
		}
		return errors.Join(fmt.Errorf("%w: the started limit stop of %s is not finished, and its release check fails; %s: %w", ErrLimitStopFailed, p.ID, remedy, err), work)
	}
	if state.ActiveWorkload == control.WorkloadIdle {
		return errors.Join(fmt.Errorf("%w: the limit stop of %s holds the error state", ErrLimitStopped, p.ID), work)
	}
	stopped, err := c.journaledLimitStop(ctx, state, p, "finish an earlier limit stop")
	switch {
	case err == nil:
		return errors.Join(fmt.Errorf("%w: finished the earlier limit stop of %s", ErrLimitStopped, p.ID), work)
	case stopped:
		return errors.Join(fmt.Errorf("%w: the earlier limit stop of %s: %w", ErrLimitStopped, p.ID, err), work)
	}
	return errors.Join(fmt.Errorf("%w: the earlier limit stop of %s: %w", ErrLimitStopFailed, p.ID, err), work)
}
