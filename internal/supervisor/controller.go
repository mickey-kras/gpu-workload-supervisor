package supervisor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

var (
	ErrUserOwned         = errors.New("GPU is in user control mode")
	ErrTransitionRunning = errors.New("another transition is running")
	ErrDrainTimeout      = errors.New("timed out waiting for admitted work")
)

type StateStore interface {
	State(context.Context) (control.State, error)
	UpdateState(context.Context, uint64, control.State) (control.State, error)
	StartTransition(context.Context, uint64, store.Transition) (control.State, error)
	SetTransitionPhase(context.Context, string, uint64, control.Phase) (control.State, error)
	FinishTransition(context.Context, string, string, uint64, control.State) (control.State, error)
	AppendTransitionEvent(context.Context, store.TransitionEvent) error
	PendingTransitionWork(context.Context, string) (int, error)
	InProgressTransition(context.Context) (string, error)
}

type Config struct {
	DrainTimeout time.Duration
	PollInterval time.Duration
}

type Controller struct {
	store   StateStore
	runtime gpuruntime.Manager
	config  Config
	now     func() time.Time
	id      func() (string, error)
}

func New(store StateStore, runtime gpuruntime.Manager, config Config) (*Controller, error) {
	return newController(store, runtime, config, time.Now, newID)
}

func newController(stateStore StateStore, runtime gpuruntime.Manager, config Config, now func() time.Time, id func() (string, error)) (*Controller, error) {
	if stateStore == nil || runtime == nil {
		return nil, errors.New("store and runtime are required")
	}
	if config.DrainTimeout <= 0 || config.PollInterval <= 0 {
		return nil, errors.New("drain timeout and poll interval must be greater than zero")
	}
	return &Controller{store: stateStore, runtime: runtime, config: config, now: now, id: id}, nil
}

func (c *Controller) Status(ctx context.Context) (control.State, error) {
	state, err := c.store.State(ctx)
	if err != nil {
		return control.State{}, err
	}
	snapshot, err := c.runtime.Observe(ctx)
	if err != nil {
		return state, fmt.Errorf("observe runtime: %w", err)
	}
	active, err := snapshot.Workload()
	if err != nil {
		state.ActiveWorkload = control.WorkloadUnknown
		state.Health = control.HealthError
		state.Admission = control.AdmissionClosed
		return state, err
	}
	state.ActiveWorkload = active
	return state, nil
}

func (c *Controller) Switch(ctx context.Context, target control.Workload, initiator string) (control.State, error) {
	if target != control.WorkloadText && target != control.WorkloadMedia && target != control.WorkloadIdle {
		return control.State{}, fmt.Errorf("invalid target workload %q", target)
	}
	current, err := c.store.State(ctx)
	if err != nil {
		return control.State{}, err
	}
	if current.Owner == control.OwnerUser {
		return current, ErrUserOwned
	}
	if running, err := c.store.InProgressTransition(ctx); err != nil {
		return current, err
	} else if running != "" {
		return current, fmt.Errorf("%w: %s", ErrTransitionRunning, running)
	}
	transitionID, err := c.id()
	if err != nil {
		return current, err
	}
	targetState := current
	targetState.DesiredWorkload = target
	transition := store.Transition{
		ID: transitionID, Source: current, Target: targetState, Previous: current,
		Initiator: initiator, Phase: control.PhaseDraining,
		Deadline: c.now().Add(c.config.DrainTimeout),
	}
	state, err := c.store.StartTransition(ctx, current.Version, transition)
	if err != nil {
		return current, err
	}
	if err := c.waitForDrain(ctx, transitionID, transition.Deadline); err != nil {
		return c.fail(ctx, transitionID, state, err)
	}
	state, err = c.setPhase(ctx, transitionID, state, control.PhaseUnloading)
	if err != nil {
		return c.fail(ctx, transitionID, state, err)
	}
	snapshot, err := c.runtime.Observe(ctx)
	if err != nil {
		return c.fail(ctx, transitionID, state, fmt.Errorf("observe before unload: %w", err))
	}
	active, err := snapshot.Workload()
	if err != nil {
		return c.fail(ctx, transitionID, state, err)
	}
	if active != control.WorkloadIdle && active != target {
		if err := c.effect(ctx, transitionID, state.Phase, "stop "+string(active), func() error {
			return c.runtime.Stop(ctx, active)
		}); err != nil {
			return c.fail(ctx, transitionID, state, err)
		}
	}
	state, err = c.setPhase(ctx, transitionID, state, control.PhaseLoading)
	if err != nil {
		return c.fail(ctx, transitionID, state, err)
	}
	if target != control.WorkloadIdle && active != target {
		if err := c.effect(ctx, transitionID, state.Phase, "start "+string(target), func() error {
			return c.runtime.Start(ctx, target)
		}); err != nil {
			return c.fail(ctx, transitionID, state, err)
		}
	}
	state, err = c.setPhase(ctx, transitionID, state, control.PhaseVerifying)
	if err != nil {
		return c.fail(ctx, transitionID, state, err)
	}
	if err := c.verify(ctx, target); err != nil {
		return c.fail(ctx, transitionID, state, err)
	}
	final := state
	final.DesiredWorkload = target
	final.ActiveWorkload = target
	final.Phase = control.PhaseStable
	final.Health = control.HealthHealthy
	if target == control.WorkloadIdle {
		final.Admission = control.AdmissionClosed
	} else {
		final.Admission = control.AdmissionOpen
	}
	return c.store.FinishTransition(ctx, transitionID, "committed", state.Version, final)
}

func (c *Controller) Reconcile(ctx context.Context) (control.State, error) {
	state, err := c.store.State(ctx)
	if err != nil {
		return control.State{}, err
	}
	snapshot, observeErr := c.runtime.Observe(ctx)
	active, workloadErr := snapshot.Workload()
	if observeErr != nil || workloadErr != nil {
		state.ActiveWorkload = control.WorkloadUnknown
		state.Phase = control.PhaseReconciling
		state.Health = control.HealthError
		state.Admission = control.AdmissionClosed
		updated, updateErr := c.store.UpdateState(ctx, state.Version, state)
		if updateErr != nil {
			return state, updateErr
		}
		return updated, errors.Join(observeErr, workloadErr)
	}
	state.Owner = control.OwnerSupervisor
	state.DesiredWorkload = active
	state.ActiveWorkload = active
	state.Phase = control.PhaseStable
	state.Health = control.HealthHealthy
	if active == control.WorkloadIdle {
		state.Admission = control.AdmissionClosed
	} else {
		if err := c.runtime.Healthy(ctx, active); err != nil {
			state.Health = control.HealthError
			state.Admission = control.AdmissionClosed
			updated, updateErr := c.store.UpdateState(ctx, state.Version, state)
			if updateErr != nil {
				return state, updateErr
			}
			return updated, err
		}
		state.Admission = control.AdmissionOpen
	}
	return c.store.UpdateState(ctx, state.Version, state)
}

func (c *Controller) waitForDrain(ctx context.Context, transitionID string, deadline time.Time) error {
	ticker := time.NewTicker(c.config.PollInterval)
	defer ticker.Stop()
	for {
		pending, err := c.store.PendingTransitionWork(ctx, transitionID)
		if err != nil {
			return err
		}
		if pending == 0 {
			return nil
		}
		if !c.now().Before(deadline) {
			return ErrDrainTimeout
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c *Controller) setPhase(ctx context.Context, transitionID string, state control.State, phase control.Phase) (control.State, error) {
	return c.store.SetTransitionPhase(ctx, transitionID, state.Version, phase)
}

func (c *Controller) effect(ctx context.Context, transitionID string, phase control.Phase, action string, fn func() error) error {
	if err := c.store.AppendTransitionEvent(ctx, store.TransitionEvent{
		TransitionID: transitionID, Phase: phase, Kind: "intent", Action: action,
	}); err != nil {
		return err
	}
	err := fn()
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	journalErr := c.store.AppendTransitionEvent(ctx, store.TransitionEvent{
		TransitionID: transitionID, Phase: phase, Kind: "observation", Action: action, Outcome: outcome,
	})
	return errors.Join(err, journalErr)
}

func (c *Controller) verify(ctx context.Context, target control.Workload) error {
	snapshot, err := c.runtime.Observe(ctx)
	if err != nil {
		return err
	}
	active, err := snapshot.Workload()
	if err != nil {
		return err
	}
	if active != target {
		return fmt.Errorf("observed workload %q, expected %q", active, target)
	}
	return c.runtime.Healthy(ctx, target)
}

func (c *Controller) fail(ctx context.Context, transitionID string, state control.State, cause error) (control.State, error) {
	final := state
	final.Phase = control.PhaseReconciling
	final.Health = control.HealthError
	final.Admission = control.AdmissionClosed
	if snapshot, err := c.runtime.Observe(ctx); err == nil {
		if active, err := snapshot.Workload(); err == nil {
			final.ActiveWorkload = active
		} else {
			final.ActiveWorkload = control.WorkloadUnknown
		}
	} else {
		final.ActiveWorkload = control.WorkloadUnknown
	}
	updated, finishErr := c.store.FinishTransition(ctx, transitionID, "failed", state.Version, final)
	return updated, errors.Join(cause, finishErr)
}

func newID() (string, error) {
	return storeNewID()
}
