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

// TransferToUser drains supervisor work before committing user ownership.
func (c *Controller) TransferToUser(ctx context.Context, target control.Workload, initiator string) (control.State, error) {
	return c.transition(ctx, target, control.OwnerSupervisor, control.OwnerUser, initiator, false)
}

// SwitchUser changes the user-owned workload; it explicitly authorizes
// stopping the current user workload.
func (c *Controller) SwitchUser(ctx context.Context, target control.Workload, initiator string) (control.State, error) {
	return c.transition(ctx, target, control.OwnerUser, control.OwnerUser, initiator, false)
}

// TransferToSupervisor returns ownership to the supervisor; it explicitly
// authorizes stopping the current user workload.
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
	preserve    bool
	operator    *control.OperatorPrecondition
}

func (c *Controller) transitionConditional(ctx context.Context, target control.Workload, initiator string, options transitionOptions) (control.State, error) {
	if !c.configuredTarget(target) {
		return control.State{}, fmt.Errorf("invalid target workload %q", target)
	}
	if err := c.checkCatalog(ctx); err != nil {
		return control.State{}, err
	}
	current, err := c.transitionSource(ctx, options)
	if err != nil {
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
		Target: stableTarget(current, options.targetOwner, target), Previous: current,
		Initiator: initiator, Phase: control.PhaseDraining,
		Deadline: c.now().Add(c.config.DrainTimeout),
	}
	if c.config.Catalog != nil {
		transition.ConfigurationRevision = c.config.Catalog.Revision
	}
	state, err := c.startRequestedTransition(ctx, current.Version, options, transition)
	if err != nil {
		return current, err
	}
	return c.executeTransition(ctx, transition, state, current, target, options)
}

func (c *Controller) transitionSource(ctx context.Context, options transitionOptions) (control.State, error) {
	current, err := c.store.State(ctx)
	if err != nil {
		return control.State{}, err
	}
	if options.operator == nil && current.Owner != options.sourceOwner {
		if current.Owner == control.OwnerUser {
			return current, ErrUserOwned
		}
		return current, ErrSupervisorOwned
	}
	if options.verifyOnly {
		closed := closedReconciling(current)
		current, err = c.store.Recover(ctx, current.Version, closed, "operator-user-recovery")
		if err != nil {
			return current, err
		}
	}
	if current.Health == control.HealthError && !options.verifyOnly {
		return current, ErrRecoveryRequired
	}
	if running, err := c.store.InProgressTransition(ctx); err != nil {
		return current, err
	} else if running != "" {
		return current, fmt.Errorf("%w: %s", ErrTransitionRunning, running)
	}
	if current.Phase != control.PhaseStable && !options.verifyOnly {
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
	unloading, err := c.store.SetTransitionPhase(ctx, transitionID, state.Version, control.PhaseUnloading)
	if err != nil {
		return state, active, err
	}
	state = unloading
	if previous.Owner == control.OwnerUser {
		// User submissions are not registered work. Terminate both runtimes,
		// including queued media jobs, before waiting for HTTP handoffs.
		err = c.effect(ctx, transitionID, state.Phase, "stop user runtimes", c.runtime.StopForRecovery)
		if err == nil {
			err = c.waitReleased(ctx, c.now().Add(c.config.VerifyTimeout))
		}
		return state, control.WorkloadIdle, err
	}
	active, err = c.unloadCatalog(ctx, transitionID, state.Phase, previous, target)
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
	if snapshot.AnyActive() {
		return gate, ErrStateVerification
	}
	return gate, nil
}

func (c *Controller) loadTransition(ctx context.Context, transitionID string, state control.State, active, target control.Workload, verifyOnly bool) (control.State, error) {
	if verifyOnly {
		return state, nil
	}
	loading, err := c.store.SetTransitionPhase(ctx, transitionID, state.Version, control.PhaseLoading)
	if err != nil {
		return state, err
	}
	state = loading
	if target != control.WorkloadIdle && active != target {
		err = c.effect(ctx, transitionID, state.Phase, "start "+string(target), func(actionCtx context.Context) error {
			return c.runtime.Start(actionCtx, target)
		})
	}
	return state, err
}

func (c *Controller) executeTransition(ctx context.Context, transition store.Transition, state, current control.State, target control.Workload, options transitionOptions) (control.State, error) {
	fail := c.fail
	if options.preserve {
		fail = c.failPreserving
	}
	skipEffects := options.verifyOnly || options.preserve
	if err := c.waitForDrain(ctx, transition.ID, transition.Deadline); err != nil {
		return fail(transition.ID, state, current, err)
	}
	state, active, err := c.unloadTransition(ctx, transition.ID, state, current, target, skipEffects)
	if err != nil {
		return fail(transition.ID, state, current, err)
	}
	userGate, err := c.acquireTransitionGate(ctx, options.sourceOwner, options.verifyOnly)
	defer userGate.Close()
	if err != nil {
		return fail(transition.ID, state, current, err)
	}
	state, err = c.loadTransition(ctx, transition.ID, state, active, target, skipEffects)
	if err != nil {
		return fail(transition.ID, state, current, err)
	}
	verifying, err := c.store.SetTransitionPhase(ctx, transition.ID, state.Version, control.PhaseVerifying)
	if err != nil {
		return fail(transition.ID, state, current, err)
	}
	state = verifying
	if err := c.waitReady(ctx, target, c.now().Add(c.config.VerifyTimeout)); err != nil {
		return fail(transition.ID, state, current, err)
	}
	final := stableTarget(state, options.targetOwner, target)
	finalizeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.config.FinalizeTimeout)
	defer cancel()
	return c.store.FinishTransition(finalizeCtx, transition.ID, "committed", state.Version, final)
}
