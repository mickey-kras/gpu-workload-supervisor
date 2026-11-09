package supervisor

import (
	"context"
	"fmt"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

// Status observes the runtime. It never starts or stops a workload. For a
// supervisor-owned, stable, healthy state with open admission, a failed
// health check, a limit failure included, latches the error state and
// closes admission. The limit stop of the policy tick or recovery can
// then stop the unit. Other states get no health check here.
func (c *Controller) Status(ctx context.Context) (control.State, error) {
	if err := c.checkCatalog(ctx); err != nil {
		return control.State{}, err
	}
	state, err := c.store.State(ctx)
	if err != nil {
		return control.State{}, err
	}
	snapshot, err := c.observe(ctx)
	if err != nil {
		cause := fmt.Errorf("observe runtime: %w", err)
		return c.statusObservationFailure(ctx, state, cause)
	}
	active, err := observedWorkload(state, snapshot)
	if err != nil {
		if state.Owner == control.OwnerUser {
			return state, err
		}
		return c.latchObservationFailure(ctx, state, err)
	}
	// Persist a closed gate when the runtime drifts or loses health while
	// admission is open. Status never starts or stops either workload.
	if state.Owner == control.OwnerSupervisor && state.Phase == control.PhaseStable &&
		state.Health == control.HealthHealthy && state.Admission == control.AdmissionOpen {
		if active != state.ActiveWorkload || (!c.configuredTarget(active) || active == control.WorkloadIdle) {
			return c.latchObservationFailure(ctx, state, ErrStateVerification)
		}
		if err := c.healthy(ctx, active); err != nil {
			return c.latchObservationFailure(ctx, state,
				gpuruntime.SafeError(ErrHealthCheck.Error(), ErrHealthCheck, err))
		}
	}
	state.ActiveWorkload = active
	return state, nil
}

func (c *Controller) statusObservationFailure(ctx context.Context, state control.State, cause error) (control.State, error) {
	if state.Owner == control.OwnerUser {
		return state, cause
	}
	return c.latchObservationFailure(ctx, state, cause)
}
