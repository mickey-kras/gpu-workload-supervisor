package supervisor

import (
	"context"
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
	snap, err := c.observe(ctx)
	if err != nil {
		return c.latchObservationFailure(ctx, state, err)
	}
	target, err := c.retainedCatalogTarget(snap)
	if err != nil {
		return c.latchObservationFailure(ctx, state, err)
	}
	return c.reconcileCatalogTarget(ctx, state, target, recovering)
}

func (c *Controller) retainedCatalogTarget(snap gpuruntime.Snapshot) (control.Workload, error) {
	target := control.WorkloadIdle
	for _, p := range c.config.Catalog.Catalog.Profiles {
		if p.BootPolicy == "retain" && snap.Workloads[p.ID].Active {
			if target != control.WorkloadIdle {
				return control.WorkloadUnknown, ErrInvariant
			}
			target = p.ID
		}
	}
	return target, nil
}

func (c *Controller) reconcileCatalogTarget(ctx context.Context, state control.State, target control.Workload, recovering bool) (control.State, error) {
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
	if !recovering && target != control.WorkloadIdle && pending == 0 && state.ActiveWorkload == target && state.DesiredWorkload == target && state.Phase == control.PhaseStable && state.Health == control.HealthHealthy && state.Admission == control.AdmissionOpen {
		if err := c.checkReady(ctx, target); err != nil {
			return c.latchObservationFailure(ctx, state, err)
		}
		return state, nil
	}
	if !recovering {
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
		return state, err
	}
	final := stableTarget(state, control.OwnerSupervisor, target)
	// The settle write must survive caller cancellation once runtime effects
	// have completed; an interrupted settle would strand the reconciliation.
	finalizeCtx, cancel := context.WithTimeout(context.Background(), c.config.FinalizeTimeout)
	defer cancel()
	return c.store.Recover(finalizeCtx, state.Version, final, "catalog-reconciliation")
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
