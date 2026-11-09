package supervisor

import (
	"context"
	"errors"
	"fmt"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

func (c *Controller) configuredTarget(id control.Workload) bool {
	if id == control.WorkloadIdle {
		return true
	}
	_, ok := c.config.Catalog.Catalog.Profile(id)
	return ok
}
func (c *Controller) checkCatalog(ctx context.Context) error {
	snap, err := c.store.Catalog(ctx)
	if err != nil {
		return err
	}
	if snap.Revision != c.config.Catalog.Revision {
		return store.ErrVersionConflict
	}
	return nil
}
func (c *Controller) unloadCatalog(ctx context.Context, id string, phase control.Phase, current control.State, target control.Workload) (control.Workload, error) {
	snap, err := c.observe(ctx)
	if err != nil {
		return control.WorkloadUnknown, err
	}
	active, err := observedWorkload(current, snap)
	if err != nil {
		return active, err
	}
	for _, p := range c.config.Catalog.Catalog.Profiles {
		if p.ID == target {
			continue
		}
		if err = c.effect(ctx, id, phase, "stop "+string(p.ID), func(ctx context.Context) error { return c.runtime.Stop(ctx, p.ID) }); err != nil {
			return active, err
		}
	}
	return active, c.waitReleasedFor(ctx, target, c.now().Add(c.config.VerifyTimeout))
}
func (c *Controller) rollbackCatalog(ctx context.Context, id string, previous control.State) error {
	_, err := c.unloadCatalog(ctx, id, control.PhaseReconciling, previous, control.WorkloadIdle)
	if err != nil {
		return err
	}
	if previous.ActiveWorkload != control.WorkloadIdle && previous.ActiveWorkload != control.WorkloadUnknown {
		if err = c.effect(ctx, id, control.PhaseReconciling, "rollback start "+string(previous.ActiveWorkload), func(ctx context.Context) error { return c.runtime.Start(ctx, previous.ActiveWorkload) }); err != nil {
			return err
		}
	}
	return c.waitReady(ctx, previous.ActiveWorkload, c.now().Add(c.config.CleanupTimeout))
}
func (c *Controller) reconcileCatalog(ctx context.Context, recovering bool) (control.State, error) {
	state, err := c.store.State(ctx)
	if err != nil {
		return state, err
	}
	if err = c.checkCatalog(ctx); err != nil {
		return state, err
	}
	if state.Owner == control.OwnerUser {
		return state, ErrUserOwned
	}
	running, err := c.store.InProgressTransition(ctx)
	if err != nil {
		return state, err
	}
	if !recovering && (running != "" || state.Health == control.HealthError) {
		return c.latchObservationFailure(ctx, state, ErrRecoveryRequired)
	}
	if recovering {
		state, err = c.beginRecovery(ctx, state)
	} else {
		if err = c.preflight(ctx); err != nil {
			return c.latchObservationFailure(ctx, state, err)
		}
	}
	if err != nil {
		return state, err
	}
	snap, err := c.observeForReconciliation(ctx, recovering)
	if err != nil {
		return c.latchObservationFailure(ctx, state, err)
	}
	retained, err := c.retainedCatalogTarget(ctx, snap)
	if err != nil {
		// This path latches and stops nothing, also the units already
		// rejected: they keep running behind the closed admission until
		// recover or the limit stop of the policy tick stops them. The
		// latch error names them with their next steps.
		// The latch needs recover, and the workload that stays is unknown.
		return c.latchObservationFailure(ctx, state, errors.Join(limitRemedy(retained.rejected, unknownLatched), err))
	}
	return c.settleCatalog(ctx, state, retained, recovering)
}

// settleCatalog settles on the retained workload and adds the next steps
// for the rejected units. Reconcile latches a rejection. A failed settle
// leaves the error state with an unknown workload, also in recover.
func (c *Controller) settleCatalog(ctx context.Context, state control.State, retained retainedTarget, recovering bool) (control.State, error) {
	settle := catalogSettle{recovering: recovering, rejected: retained.rejected != nil}
	state, err := c.reconcileCatalogTarget(ctx, state, retained.workload, settle)
	from := remedyState{latched: !recovering || err != nil, kept: retained.workload}
	if err != nil {
		from.kept = control.WorkloadUnknown
	}
	return state, errors.Join(limitRemedy(retained.rejected, from), err)
}

// Recovery may inspect failed units to choose what to retain, but all later
// readiness observations and release checks keep their normal strict rules.
func (c *Controller) observeForReconciliation(ctx context.Context, recovering bool) (gpuruntime.Snapshot, error) {
	if observer, ok := c.runtime.(interface {
		ObserveForRecovery(context.Context) (gpuruntime.Snapshot, error)
	}); recovering && ok {
		probeCtx, cancel := context.WithTimeout(ctx, c.config.ActionTimeout)
		defer cancel()
		return observer.ObserveForRecovery(probeCtx)
	}
	return c.observe(ctx)
}

// ErrLimitedUnitRejected reports a running unit with limits that failed a
// definite check. Reconciliation does not keep it, so it stops the unit like
// any unit it does not keep. Reconcile latches the error state in the same
// write that settles the state. Recover does not latch it again: recovery is
// the explicit operator action, and it has stopped the unit. Recover still
// returns the error, so the operator sees which unit it stopped and why.
var ErrLimitedUnitRejected = errors.New("a running unit with resource limits failed a check")

// catalogSettle tells reconcileCatalogTarget how to settle. rejected tells
// that a unit with limits failed a definite check; settleCatalog reports
// which. Such a unit can still run, so a rejection always takes the full
// path, which stops every unit except the target.
type catalogSettle struct {
	recovering bool
	rejected   bool
}

// final gives the settled state. After a rejection, reconciliation keeps
// the failure latched with error health and closed admission until
// explicit recovery. The active workload stays the one that reconciliation
// settled on. The state row keeps no cause: the error that reconcile
// returns names it.
func (s catalogSettle) final(state control.State, target control.Workload) control.State {
	final := stableTarget(state, control.OwnerSupervisor, target)
	if s.rejected && !s.recovering {
		return closedReconciling(final)
	}
	return final
}

// retainedTarget is the running unit that reconciliation keeps. rejected
// names each unit with limits that failed a definite check: it is not kept,
// so reconciliation stops it, and the caller reports why.
type retainedTarget struct {
	workload control.Workload
	rejected error
}

func (c *Controller) retainedCatalogTarget(ctx context.Context, snap gpuruntime.Snapshot) (retainedTarget, error) {
	retained := retainedTarget{workload: control.WorkloadIdle}
	for _, p := range c.config.Catalog.Catalog.Profiles {
		if p.BootPolicy != "retain" || !snap.Workloads[p.ID].Active {
			continue
		}
		if checkErr := c.checkRetainedLimits(ctx, p); checkErr != nil {
			if !gpuruntime.DefiniteLimitFailure(checkErr) {
				// A check that proves nothing stops the loop: the later
				// units stay unchecked. Return the units already proven
				// bad with the error, so the latch error names them with
				// their next steps.
				return retainedTarget{rejected: retained.rejected}, gpuruntime.UnitError(p, checkErr)
			}
			retained.rejected = errors.Join(retained.rejected, limitedUnitError(ErrLimitedUnitRejected, p, checkErr))
			continue
		}
		if retained.workload != control.WorkloadIdle {
			return retainedTarget{rejected: retained.rejected}, ErrInvariant
		}
		retained.workload = p.ID
	}
	return retained, nil
}

// checkRetainedLimits runs the binding check, the host protection checks
// and the cgroup readback before reconciliation keeps a running unit with
// limits. It leaves out the slice memory check: the running unit already
// holds its memory. A runtime that cannot run the checks fails them.
func (c *Controller) checkRetainedLimits(ctx context.Context, p control.WorkloadProfile) error {
	if !p.HasLimits() {
		return nil
	}
	return c.runLimitProbe(ctx, p.ID, limitChecker.VerifyLimitProtection)
}

// limitedUnitError names the unit with limits that err refuses. The class
// already says that a check failed, so the text leaves out the started-unit
// prefix of err. The error keeps that class.
func limitedUnitError(class error, p control.WorkloadProfile, err error) error {
	if cause, ok := gpuruntime.StartedCause(err); ok {
		err = gpuruntime.SafeError(cause.Error(), err)
	}
	return fmt.Errorf("%w: %w", class, gpuruntime.UnitError(p, err))
}

// limitChecker runs the checks of a unit with limits: the launch checks
// before any unload, and the checks of a running unit. A runtime without
// it refuses or fails such a unit.
type limitChecker interface {
	VerifyLaunchLimits(context.Context, control.Workload) error
	VerifyLimitProtection(context.Context, control.Workload) error
}

// runLimitProbe runs one runtime limit check within ActionTimeout. A
// runtime without the checks fails it with ErrLimitUnchecked.
func (c *Controller) runLimitProbe(ctx context.Context, id control.Workload, probe func(limitChecker, context.Context, control.Workload) error) error {
	checker, ok := c.runtime.(limitChecker)
	if !ok {
		return gpuruntime.ErrLimitUnchecked
	}
	probeCtx, cancel := context.WithTimeout(ctx, c.config.ActionTimeout)
	defer cancel()
	return probe(checker, probeCtx, id)
}

// unknownLatched is the remedy state of a latch: the state needs recovery,
// and the running workload is unknown.
var unknownLatched = remedyState{latched: true, kept: control.WorkloadUnknown}

func (c *Controller) reconcileCatalogTarget(ctx context.Context, state control.State, target control.Workload, settle catalogSettle) (control.State, error) {
	pendingOpposing := func(ctx context.Context) (int, error) {
		if target == control.WorkloadIdle {
			return c.store.PendingWork(ctx)
		}
		return c.store.PendingWorkExcept(ctx, target)
	}
	pending, err := pendingOpposing(ctx)
	if err != nil {
		return c.latchObservationFailure(ctx, state, err)
	}
	if !settle.recovering && !settle.rejected && target != control.WorkloadIdle && pending == 0 && state.ActiveWorkload == target && state.DesiredWorkload == target && state.Phase == control.PhaseStable && state.Health == control.HealthHealthy && state.Admission == control.AdmissionOpen {
		if err := c.checkReady(ctx, target); err != nil {
			return c.latchObservationFailure(ctx, state, c.stopFailedLimits(ctx, target, err))
		}
		return state, nil
	}
	if !settle.recovering {
		state, err = c.enterReconciliation(ctx, state)
		if err != nil {
			return state, err
		}
	}
	if err := c.waitForWork(ctx, c.now().Add(c.config.DrainTimeout), pendingOpposing); err != nil {
		return state, err
	}
	if err := c.stopOpposingCatalog(ctx, target); err != nil {
		return state, err
	}
	if err := c.waitReleasedFor(ctx, target, c.now().Add(c.config.VerifyTimeout)); err != nil {
		return state, err
	}
	if err := c.waitReady(ctx, target, c.now().Add(c.config.VerifyTimeout)); err != nil {
		return state, c.stopFailedLimits(ctx, target, err)
	}
	// One write settles the state and latches a rejection, so a crash
	// cannot keep the settle without the latch.
	final := settle.final(state, target)
	// The settle write must survive caller cancellation once runtime effects
	// have completed; an interrupted settle would strand the reconciliation.
	finalizeCtx, cancel := context.WithTimeout(context.Background(), c.config.FinalizeTimeout)
	defer cancel()
	return c.store.Recover(finalizeCtx, state.Version, final, "catalog-reconciliation")
}

// stopFailedLimits stops the kept target when its readiness probe found a
// definite limit failure: reconciliation does not keep such a unit. The
// state is already latched or about to latch. Like the other stops of
// reconciliation, it is not journaled. It runs on a detached, bounded
// context, because the reconcile context can be spent.
func (c *Controller) stopFailedLimits(ctx context.Context, target control.Workload, err error) error {
	if !errors.Is(err, gpuruntime.ErrLimitCheckFailed) {
		return err
	}
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.config.CleanupTimeout)
	defer cancel()
	return errors.Join(err, c.runAction(stopCtx, func(ctx context.Context) error { return c.runtime.Stop(ctx, target) }))
}

func (c *Controller) stopOpposingCatalog(ctx context.Context, target control.Workload) error {
	for _, p := range c.config.Catalog.Catalog.Profiles {
		if p.ID != target {
			if err := c.runAction(ctx, func(ctx context.Context) error { return c.runtime.Stop(ctx, p.ID) }); err != nil {
				return err
			}
		}
	}
	return nil
}
