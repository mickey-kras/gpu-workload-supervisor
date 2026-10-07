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

// ActivateWorkload is the policy-driven workload-start path: it requires an
// idle, admission-closed, supervisor-owned state with no unfinished admitted
// work, all revalidated inside the store writer transaction. The committed
// fence rotation authorizes exactly one execution generation.
func (c *Controller) ActivateWorkload(ctx context.Context, target control.Workload, e control.OperatorPrecondition) (control.State, error) {
	if e.Owner != control.OwnerSupervisor {
		return control.State{}, store.ErrWrongOwner
	}
	if target == control.WorkloadIdle || target == control.WorkloadUnknown || !c.configuredTarget(target) {
		return control.State{}, fmt.Errorf("invalid target workload %q", target)
	}
	if err := c.checkCatalog(ctx); err != nil {
		return control.State{}, err
	}
	current, err := c.store.State(ctx)
	if err != nil {
		return control.State{}, err
	}
	if err := c.store.CheckActivationPrecondition(ctx, e); err != nil {
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

const policyActivationInitiator = "policy-activation"
