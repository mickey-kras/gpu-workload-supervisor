package supervisor

import (
	"context"
	"errors"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

// inactivityPolicyInitiator names the journaled transition of an idle stop.
const inactivityPolicyInitiator = "inactivity-policy"

// IdleTick runs the inactivity policy once. With the policy on, it
// stops the active workload to Idle when no activity happened for the
// timeout. It acts only on a healthy, stable, supervisor-owned workload with
// open admission, and never on a trainer. A state that does not qualify is a
// clean no-op, as is a stop that activity preempts. The store checks every
// precondition again in the transaction that starts the stop, so the tick
// keeps no state of its own and is safe across restarts.
func (c *Controller) IdleTick(ctx context.Context) error {
	settings, err := c.store.Settings(ctx)
	if err != nil {
		return err
	}
	if settings.Policy.TimeoutMinutes == control.IdlePolicyOff || settings.LastActivityAt == nil {
		return nil
	}
	current, err := c.store.State(ctx)
	if err != nil {
		return err
	}
	if current.Owner != control.OwnerSupervisor || current.Phase != control.PhaseStable ||
		current.Health != control.HealthHealthy || current.Admission != control.AdmissionOpen ||
		current.ActiveWorkload == control.WorkloadIdle || current.ActiveWorkload == control.WorkloadUnknown {
		return nil
	}
	// A trainer is a finite job: only its own terminal path ends it.
	if p, ok := c.config.Catalog.Catalog.Profile(current.ActiveWorkload); ok && p.Trainer != nil {
		return nil
	}
	deadline := settings.LastActivityAt.Add(time.Duration(settings.Policy.TimeoutMinutes) * time.Minute)
	if c.now().Before(deadline) {
		return nil
	}
	if err := c.checkCatalog(ctx); err != nil {
		return err
	}
	if err := c.preflightForStop(ctx); err != nil {
		return err
	}
	transitionID, err := c.id()
	if err != nil {
		return err
	}
	transition := store.Transition{
		ID: transitionID, Source: current,
		Target: stableTarget(current, control.OwnerSupervisor, control.WorkloadIdle), Previous: current,
		Initiator: inactivityPolicyInitiator, Phase: control.PhaseDraining,
		Deadline: c.now().Add(c.config.DrainTimeout),
	}
	transition.ConfigurationRevision = c.config.Catalog.Revision
	state, err := c.store.StartIdleTransition(ctx, transition)
	if errors.Is(err, store.ErrPolicyPreempted) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = c.executeTransition(ctx, transition, state, current, control.WorkloadIdle, transitionOptions{
		sourceOwner: control.OwnerSupervisor, targetOwner: control.OwnerSupervisor,
	})
	return err
}
