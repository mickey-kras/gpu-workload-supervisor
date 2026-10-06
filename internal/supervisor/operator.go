package supervisor

import (
	"context"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

const takeControlOperation = "take-control"

// OperatorTransition is the local account's conditional ownership path.
func (c *Controller) OperatorTransition(ctx context.Context, action string, target control.Workload, e control.OperatorPrecondition) (control.State, error) {
	s, err := c.store.State(ctx)
	if err != nil {
		return s, err
	}
	if e.Incarnation == "" || e.Incarnation != s.LeaseFence.Incarnation || e.Version != s.Version {
		return s, store.ErrVersionConflict
	}
	if e.Owner != s.Owner {
		return s, store.ErrWrongOwner
	}
	source, dest := control.OwnerUser, control.OwnerUser
	switch action {
	case takeControlOperation:
		source = control.OwnerSupervisor
	case "user-switch":
	case "return-control":
		dest = control.OwnerSupervisor
		target = control.WorkloadIdle
	default:
		return s, errors.New("invalid operator action")
	}
	if source != s.Owner {
		return s, store.ErrWrongOwner
	}
	if action == takeControlOperation {
		verified, err := c.Status(ctx)
		if err != nil {
			return verified, err
		}
		if verified.Version != e.Version {
			return verified, store.ErrVersionConflict
		}
		target = verified.ActiveWorkload
		if err = c.checkReady(ctx, target); err != nil {
			return verified, err
		}
	}
	return c.transitionConditional(ctx, target, "local-operator", transitionOptions{sourceOwner: source, targetOwner: dest, operator: &e, preserve: action == takeControlOperation})
}

func (c *Controller) startRequestedTransition(ctx context.Context, version uint64, o transitionOptions, t store.Transition) (control.State, error) {
	if o.operator == nil {
		return c.store.StartTransition(ctx, version, t)
	}
	return c.store.StartOperatorTransition(ctx, *o.operator, t)
}

// failPreserving never executes lifecycle rollback: takeover has performed only
// observations and work draining. Existing admitted work must not be restarted
// merely because the drain or subsequent verification failed.
func (c *Controller) failPreserving(id string, state, previous control.State, cause error) (control.State, error) {
	final := closedReconciling(state)
	final.Owner = previous.Owner
	final.ActiveWorkload = control.WorkloadUnknown
	ctx, cancel := context.WithTimeout(context.Background(), c.config.FinalizeTimeout)
	defer cancel()
	journalErr := c.store.AppendTransitionEvent(ctx, store.TransitionEvent{TransitionID: id, Phase: final.Phase, Kind: "observation", Action: "takeover failed without runtime effects", Outcome: failureCode(cause)})
	updated, err := c.store.FinishTransition(ctx, id, "failed", state.Version, final)
	return updated, errors.Join(cause, journalErr, err)
}
