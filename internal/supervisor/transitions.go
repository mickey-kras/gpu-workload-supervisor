package supervisor

import (
	"context"
	"fmt"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

// Switch changes the supervisor workload; it never claims user-owned hardware.
func (c *Controller) Switch(ctx context.Context, target control.Workload, initiator string) (control.State, error) {
	return c.transition(ctx, target, control.OwnerSupervisor, control.OwnerSupervisor, initiator, false)
}

// SwitchConditional preserves a caller's observed state token through the
// authoritative transition transaction. It cannot transfer ownership.
func (c *Controller) SwitchConditional(ctx context.Context, target control.Workload, initiator string, expected control.Precondition) (control.State, error) {
	return c.transitionConditional(ctx, target, initiator, transitionOptions{
		sourceOwner: control.OwnerSupervisor, targetOwner: control.OwnerSupervisor, expected: &expected,
	})
}

// TransferToUser drains supervisor work before committing user ownership.
func (c *Controller) TransferToUser(ctx context.Context, target control.Workload, initiator string) (control.State, error) {
	return c.transition(ctx, target, control.OwnerSupervisor, control.OwnerUser, initiator, false)
}

// SwitchUser explicitly authorizes stopping the current user workload.
func (c *Controller) SwitchUser(ctx context.Context, target control.Workload, initiator string) (control.State, error) {
	return c.transition(ctx, target, control.OwnerUser, control.OwnerUser, initiator, false)
}

// TransferToSupervisor explicitly authorizes stopping the current user workload.
func (c *Controller) TransferToSupervisor(ctx context.Context, target control.Workload, initiator string) (control.State, error) {
	return c.transition(ctx, target, control.OwnerUser, control.OwnerSupervisor, initiator, false)
}

// RecoverUser verifies an operator-selected workload without starting or stopping
// user work. Interrupted transitions remain failed; ownership stays with the user.
func (c *Controller) RecoverUser(ctx context.Context, target control.Workload, initiator string) (control.State, error) {
	return c.transition(ctx, target, control.OwnerUser, control.OwnerUser, initiator, true)
}

func (c *Controller) transition(ctx context.Context, target control.Workload, sourceOwner, targetOwner control.Owner, initiator string, verifyOnly bool) (control.State, error) {
	return c.transitionConditional(ctx, target, initiator, transitionOptions{
		sourceOwner: sourceOwner, targetOwner: targetOwner, verifyOnly: verifyOnly,
	})
}

type transitionOptions struct {
	sourceOwner control.Owner
	targetOwner control.Owner
	verifyOnly  bool
	expected    *control.Precondition
}

func (c *Controller) transitionConditional(ctx context.Context, target control.Workload, initiator string, options transitionOptions) (control.State, error) {
	if target != control.WorkloadText && target != control.WorkloadMedia && target != control.WorkloadIdle {
		return control.State{}, fmt.Errorf("invalid target workload %q", target)
	}
	current, err := c.transitionSource(ctx, options.sourceOwner, options.verifyOnly)
	if err != nil {
		return current, err
	}
	if options.expected != nil && (options.expected.Incarnation == "" || options.expected.Incarnation != current.LeaseFence.Incarnation || options.expected.Version != current.Version) {
		return current, store.ErrVersionConflict
	}
	transitionID, err := c.id()
	if err != nil {
		return current, err
	}
	transition := store.Transition{
		ID: transitionID, Source: current,
		Target: stableTarget(current, options.targetOwner, target), Previous: current,
		Initiator: initiator, Phase: control.PhaseDraining,
		Deadline: c.now().Add(c.config.DrainTimeout),
	}
	state, err := c.startTransition(ctx, current.Version, options.expected, transition)
	if err != nil {
		return current, err
	}
	if err := c.waitForDrain(ctx, transitionID, transition.Deadline); err != nil {
		return c.fail(transitionID, state, current, err)
	}
	state, active, err := c.unloadTransition(ctx, transitionID, state, current, target, options.verifyOnly)
	if err != nil {
		return c.fail(transitionID, state, current, err)
	}
	userGate, err := c.acquireTransitionGate(ctx, options.sourceOwner, options.verifyOnly)
	defer userGate.Close()
	if err != nil {
		return c.fail(transitionID, state, current, err)
	}
	state, err = c.loadTransition(ctx, transitionID, state, active, target, options.verifyOnly)
	if err != nil {
		return c.fail(transitionID, state, current, err)
	}
	state, err = c.setPhase(ctx, transitionID, state, control.PhaseVerifying)
	if err != nil {
		return c.fail(transitionID, state, current, err)
	}
	if err := c.waitReady(ctx, target, c.now().Add(c.config.VerifyTimeout)); err != nil {
		return c.fail(transitionID, state, current, err)
	}
	final := stableTarget(state, options.targetOwner, target)
	return c.store.FinishTransition(ctx, transitionID, "committed", state.Version, final)
}

type conditionalStore interface {
	StartTransitionConditional(context.Context, control.Precondition, store.Transition) (control.State, error)
}

func (c *Controller) startTransition(ctx context.Context, version uint64, expected *control.Precondition, transition store.Transition) (control.State, error) {
	if expected == nil {
		return c.store.StartTransition(ctx, version, transition)
	}
	stateStore, ok := c.store.(conditionalStore)
	if !ok {
		return control.State{}, fmt.Errorf("store does not support conditional transitions")
	}
	return stateStore.StartTransitionConditional(ctx, *expected, transition)
}

func (c *Controller) transitionSource(ctx context.Context, sourceOwner control.Owner, verifyOnly bool) (control.State, error) {
	current, err := c.store.State(ctx)
	if err != nil {
		return control.State{}, err
	}
	if current.Owner != sourceOwner {
		if current.Owner == control.OwnerUser {
			return current, ErrUserOwned
		}
		return current, ErrSupervisorOwned
	}
	if verifyOnly {
		closed := current
		closed.Phase = control.PhaseReconciling
		closed.Health = control.HealthError
		closed.Admission = control.AdmissionClosed
		current, err = c.store.Recover(ctx, current.Version, closed, "operator-user-recovery")
		if err != nil {
			return current, err
		}
	}
	if current.Health == control.HealthError && !verifyOnly {
		return current, ErrRecoveryRequired
	}
	if running, err := c.store.InProgressTransition(ctx); err != nil {
		return current, err
	} else if running != "" {
		return current, fmt.Errorf("%w: %s", ErrTransitionRunning, running)
	}
	if current.Phase != control.PhaseStable && !verifyOnly {
		return current, ErrReconcileRequired
	}
	return current, nil
}

func stableTarget(state control.State, owner control.Owner, target control.Workload) control.State {
	state.Owner = owner
	state.DesiredWorkload = target
	state.ActiveWorkload = target
	state.Phase = control.PhaseStable
	state.Health = control.HealthHealthy
	state.Admission = control.AdmissionClosed
	if owner == control.OwnerSupervisor && target != control.WorkloadIdle {
		state.Admission = control.AdmissionOpen
	}
	return state
}

func (c *Controller) unloadTransition(ctx context.Context, transitionID string, state, previous control.State, target control.Workload, verifyOnly bool) (control.State, control.Workload, error) {
	active := previous.ActiveWorkload
	if verifyOnly {
		return state, active, nil
	}
	state, err := c.setPhase(ctx, transitionID, state, control.PhaseUnloading)
	if err != nil {
		return state, active, err
	}
	if previous.Owner == control.OwnerUser {
		// User submissions are not registered work. Terminate both runtimes,
		// including queued media jobs, before waiting for HTTP handoffs.
		err = c.effect(ctx, transitionID, state.Phase, "stop user runtimes", c.runtime.StopForRecovery)
		if err == nil {
			err = c.waitReleased(ctx, c.now().Add(c.config.VerifyTimeout))
		}
		return state, control.WorkloadIdle, err
	}
	active, err = c.unloadForSwitch(ctx, transitionID, state.Phase, previous, target)
	return state, active, err
}

func (c *Controller) acquireTransitionGate(ctx context.Context, sourceOwner control.Owner, verifyOnly bool) (*lock.File, error) {
	if sourceOwner != control.OwnerUser {
		return nil, nil
	}
	gateCtx, cancel := context.WithTimeout(ctx, c.config.DrainTimeout)
	gate, err := c.store.AcquireUserExecution(gateCtx, false)
	cancel()
	if err != nil {
		return gate, fmt.Errorf("drain user request handoffs: %w", err)
	}
	if verifyOnly {
		return gate, nil
	}
	snapshot, err := c.observe(ctx)
	if err != nil {
		return gate, err
	}
	if snapshot.TextActive || snapshot.MediaReady {
		return gate, ErrStateVerification
	}
	return gate, nil
}

func (c *Controller) loadTransition(ctx context.Context, transitionID string, state control.State, active, target control.Workload, verifyOnly bool) (control.State, error) {
	if verifyOnly {
		return state, nil
	}
	state, err := c.setPhase(ctx, transitionID, state, control.PhaseLoading)
	if err != nil {
		return state, err
	}
	if target != control.WorkloadIdle && active != target {
		err = c.effect(ctx, transitionID, state.Phase, "start "+string(target), func(actionCtx context.Context) error {
			return c.runtime.Start(actionCtx, target)
		})
	}
	return state, err
}

func (c *Controller) unloadForSwitch(ctx context.Context, transitionID string, phase control.Phase, current control.State, target control.Workload) (control.Workload, error) {
	snapshot, err := c.observe(ctx)
	if err != nil {
		return control.WorkloadUnknown, fmt.Errorf("%w: observe before unload: %v", ErrRuntimeObservation, err)
	}
	active, err := observedWorkload(current, snapshot)
	if err != nil {
		return control.WorkloadUnknown, err
	}
	if active != control.WorkloadIdle && active != target {
		if err := c.effect(ctx, transitionID, phase, "stop "+string(active), func(actionCtx context.Context) error {
			return c.runtime.Stop(actionCtx, active)
		}); err != nil {
			return active, err
		}
	}
	if active != target && (active != control.WorkloadIdle || target == control.WorkloadMedia || snapshot.MediaExclusive && target != control.WorkloadText) {
		if err := c.waitReleasedFor(ctx, target, c.now().Add(c.config.VerifyTimeout)); err != nil {
			return active, err
		}
	}
	if target == control.WorkloadText && active == control.WorkloadIdle {
		return active, c.releaseMediaForText(ctx, transitionID, phase)
	}
	return active, nil
}

func (c *Controller) releaseMediaForText(ctx context.Context, transitionID string, phase control.Phase) error {
	if err := c.effect(ctx, transitionID, phase, "release media", func(actionCtx context.Context) error {
		return c.runtime.Stop(actionCtx, control.WorkloadMedia)
	}); err != nil {
		return err
	}
	return c.waitReleased(ctx, c.now().Add(c.config.VerifyTimeout))
}
