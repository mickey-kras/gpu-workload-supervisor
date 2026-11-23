package supervisor

import (
	"context"
	"fmt"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

// Activation deferrals: the local account asked for a workload while the
// supervisor could not start it right now; the request may be retried.
var (
	ErrNotIdle        = store.ErrNotIdle
	ErrUnresolvedWork = store.ErrUnresolvedWork
)

// ActivateWorkload is the policy-driven workload-start path. It starts from
// an idle, admission-closed state, or switches from another stable workload
// with open admission. Either source must be supervisor-owned with no
// unfinished admitted work. The store writer transaction checks all of this
// again. A switch never leaves a trainer: only its own terminal path ends
// it. The committed fence rotation authorizes exactly one execution
// generation.
func (c *Controller) ActivateWorkload(ctx context.Context, target control.Workload, e control.OperatorPrecondition) (control.State, error) {
	if err := c.checkActivationTarget(ctx, target, e); err != nil {
		return control.State{}, err
	}
	current, err := c.store.State(ctx)
	if err != nil {
		return control.State{}, err
	}
	// The writer transaction requires e.Version, so this check of current
	// holds for the state that the transaction checks.
	if p, ok := c.config.Catalog.Catalog.Profile(current.ActiveWorkload); ok && p.Trainer != nil {
		return current, ErrNotIdle
	}
	if err := c.store.CheckActivationPrecondition(ctx, e, target); err != nil {
		return current, err
	}
	if err := c.refuseModelCheck(ctx, target, false); err != nil {
		return current, err
	}
	if err := c.refuseGPUInUse(ctx, target); err != nil {
		return current, err
	}
	if err := c.preflight(ctx); err != nil {
		return current, err
	}
	transitionID, err := c.id()
	if err != nil {
		return current, err
	}
	transition := store.Transition{
		ID: transitionID, Source: current,
		Target: stableTarget(current, control.OwnerSupervisor, target), Previous: current,
		Initiator: policyActivationInitiator, Phase: control.PhaseDraining,
		Deadline: c.now().Add(c.config.DrainTimeout),
	}
	transition.ConfigurationRevision = c.config.Catalog.Revision
	state, err := c.store.StartActivationTransition(ctx, e, transition)
	if err != nil {
		return current, err
	}
	return c.executeTransition(ctx, transition, state, current, target, transitionOptions{
		sourceOwner: control.OwnerSupervisor, targetOwner: control.OwnerSupervisor,
	})
}

// checkActivationTarget refuses a request that no state could satisfy: a
// caller that is not the supervisor, an unconfigured or trainer target, an
// invalid catalog or a target over its limits.
func (c *Controller) checkActivationTarget(ctx context.Context, target control.Workload, e control.OperatorPrecondition) error {
	if e.Owner != control.OwnerSupervisor {
		return store.ErrWrongOwner
	}
	if target == control.WorkloadIdle || target == control.WorkloadUnknown || !c.configuredTarget(target) {
		return fmt.Errorf("%w %q", ErrInvalidTarget, target)
	}
	if p, ok := c.config.Catalog.Catalog.Profile(target); ok && p.Trainer != nil {
		return ErrTrainerTarget
	}
	if err := c.checkCatalog(ctx); err != nil {
		return err
	}
	return c.refuseLimitedTarget(ctx, target)
}

const policyActivationInitiator = "policy-activation"
