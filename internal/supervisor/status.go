package supervisor

import (
	"context"
	"fmt"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

func (c *Controller) Status(ctx context.Context) (control.State, error) {
	state, err := c.store.State(ctx)
	if err != nil {
		return control.State{}, err
	}
	snapshot, err := c.observe(ctx)
	if err != nil {
		cause := fmt.Errorf("observe runtime: %w", err)
		if state.Owner == control.OwnerUser {
			return state, cause
		}
		return c.latchObservationFailure(ctx, state, cause)
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
		if active != state.ActiveWorkload || (active != control.WorkloadText && active != control.WorkloadMedia) {
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
