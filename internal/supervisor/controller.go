package supervisor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

var (
	ErrUserOwned         = errors.New("GPU is in user control mode")
	ErrTransitionRunning = errors.New("another transition is running")
	ErrDrainTimeout      = errors.New("timed out waiting for admitted work")
	ErrVerifyTimeout     = errors.New("timed out verifying workload")
	ErrRecoveryRequired  = errors.New("explicit recovery is required")
	ErrInvariant         = errors.New("observed runtime violates control state")
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
	Recover(context.Context, uint64, control.State, string) (control.State, error)
}

type Config struct {
	DrainTimeout  time.Duration
	VerifyTimeout time.Duration
	CleanupTimeout time.Duration
	PollInterval  time.Duration
}

type Controller struct {
	store   StateStore
	runtime gpuruntime.Manager
	config  Config
	now     func() time.Time
	id      func() (string, error)
}

func New(stateStore StateStore, runtime gpuruntime.Manager, config Config) (*Controller, error) {
	return newController(stateStore, runtime, config, time.Now, func() (string, error) {
		value, err := uuid.NewRandom()
		return value.String(), err
	})
}

func newController(stateStore StateStore, runtime gpuruntime.Manager, config Config, now func() time.Time, id func() (string, error)) (*Controller, error) {
	if stateStore == nil || runtime == nil {
		return nil, errors.New("store and runtime are required")
	}
	if config.DrainTimeout <= 0 || config.VerifyTimeout <= 0 || config.CleanupTimeout <= 0 || config.PollInterval <= 0 {
		return nil, errors.New("timeouts and poll interval must be greater than zero")
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
	active, err := observedWorkload(state, snapshot)
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
	if current.Health == control.HealthError {
		return current, ErrRecoveryRequired
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
		return c.fail(transitionID, state, current, err)
	}
	state, err = c.setPhase(ctx, transitionID, state, control.PhaseUnloading)
	if err != nil {
		return c.fail(transitionID, state, current, err)
	}
	snapshot, err := c.runtime.Observe(ctx)
	if err != nil {
		return c.fail(transitionID, state, current, fmt.Errorf("observe before unload: %w", err))
	}
	active, err := observedWorkload(current, snapshot)
	if err != nil {
		return c.fail(transitionID, state, current, err)
	}
	if active != control.WorkloadIdle && active != target {
		if err := c.effect(ctx, transitionID, state.Phase, "stop "+string(active), func() error {
			return c.runtime.Stop(ctx, active)
		}); err != nil {
			return c.fail(transitionID, state, current, err)
		}
	}
	state, err = c.setPhase(ctx, transitionID, state, control.PhaseLoading)
	if err != nil {
		return c.fail(transitionID, state, current, err)
	}
	if target != control.WorkloadIdle && active != target {
		if err := c.effect(ctx, transitionID, state.Phase, "start "+string(target), func() error {
			return c.runtime.Start(ctx, target)
		}); err != nil {
			return c.fail(transitionID, state, current, err)
		}
	}
	state, err = c.setPhase(ctx, transitionID, state, control.PhaseVerifying)
	if err != nil {
		return c.fail(transitionID, state, current, err)
	}
	if err := c.waitReady(ctx, target, c.now().Add(c.config.VerifyTimeout)); err != nil {
		return c.fail(transitionID, state, current, err)
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
	if state.Owner == control.OwnerUser {
		return state, ErrUserOwned
	}
	running, err := c.store.InProgressTransition(ctx)
	if err != nil {
		return state, err
	}
	if running != "" || state.Health == control.HealthError {
		final := state
		final.ActiveWorkload = control.WorkloadUnknown
		final.Phase = control.PhaseReconciling
		final.Health = control.HealthError
		final.Admission = control.AdmissionClosed
		reason := "latched-error"
		if running != "" {
			reason = "interrupted-transition"
		}
		recovered, recoverErr := c.store.Recover(ctx, state.Version, final, reason)
		return recovered, errors.Join(ErrRecoveryRequired, recoverErr)
	}
	snapshot, err := c.runtime.Observe(ctx)
	if err != nil {
		return c.latchObservationFailure(ctx, state, err)
	}
	state.Owner = control.OwnerSupervisor
	state.Phase = control.PhaseStable
	state.Health = control.HealthHealthy
	if snapshot.TextActive {
		if err := c.runtime.Healthy(ctx, control.WorkloadText); err != nil {
			return c.latchObservationFailure(ctx, state, err)
		}
		state.DesiredWorkload = control.WorkloadText
		state.ActiveWorkload = control.WorkloadText
		state.Admission = control.AdmissionOpen
	} else {
		state.DesiredWorkload = control.WorkloadIdle
		state.ActiveWorkload = control.WorkloadIdle
		state.Admission = control.AdmissionClosed
	}
	return c.store.UpdateState(ctx, state.Version, state)
}

func (c *Controller) Recover(ctx context.Context) (control.State, error) {
	state, err := c.store.State(ctx)
	if err != nil {
		return control.State{}, err
	}
	if state.Owner == control.OwnerUser {
		return state, ErrUserOwned
	}
	snapshot, err := c.runtime.Observe(ctx)
	if err != nil {
		return state, err
	}
	final := state
	final.Owner = control.OwnerSupervisor
	final.Phase = control.PhaseStable
	final.Health = control.HealthHealthy
	if snapshot.TextActive {
		if err := c.runtime.Healthy(ctx, control.WorkloadText); err != nil {
			return state, err
		}
		final.DesiredWorkload = control.WorkloadText
		final.ActiveWorkload = control.WorkloadText
		final.Admission = control.AdmissionOpen
	} else {
		final.DesiredWorkload = control.WorkloadIdle
		final.ActiveWorkload = control.WorkloadIdle
		final.Admission = control.AdmissionClosed
	}
	return c.store.Recover(ctx, state.Version, final, "operator-recovery")
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

func (c *Controller) waitReady(ctx context.Context, target control.Workload, deadline time.Time) error {
	ticker := time.NewTicker(c.config.PollInterval)
	defer ticker.Stop()
	var lastErr error
	for {
		snapshot, err := c.runtime.Observe(ctx)
		if err == nil {
			err = verifySnapshot(target, snapshot)
		}
		if err == nil {
			err = c.runtime.Healthy(ctx, target)
		}
		if err == nil {
			return nil
		}
		lastErr = err
		if !c.now().Before(deadline) {
			return errors.Join(ErrVerifyTimeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func verifySnapshot(target control.Workload, snapshot gpuruntime.Snapshot) error {
	switch target {
	case control.WorkloadText:
		if !snapshot.TextActive {
			return errors.New("text runtime is not active")
		}
	case control.WorkloadMedia:
		if snapshot.TextActive || !snapshot.MediaReady {
			return errors.New("media runtime is not exclusively ready")
		}
	case control.WorkloadIdle:
		if snapshot.TextActive {
			return errors.New("text runtime remains active")
		}
	}
	return nil
}

func observedWorkload(state control.State, snapshot gpuruntime.Snapshot) (control.Workload, error) {
	if snapshot.TextActive {
		if state.ActiveWorkload == control.WorkloadMedia {
			return control.WorkloadUnknown, ErrInvariant
		}
		return control.WorkloadText, nil
	}
	if state.ActiveWorkload == control.WorkloadMedia && snapshot.MediaReady {
		return control.WorkloadMedia, nil
	}
	return control.WorkloadIdle, nil
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
		outcome = failureCode(err)
	}
	journalErr := c.store.AppendTransitionEvent(ctx, store.TransitionEvent{
		TransitionID: transitionID, Phase: phase, Kind: "observation", Action: action, Outcome: outcome,
	})
	return errors.Join(err, journalErr)
}

func (c *Controller) fail(transitionID string, state, previous control.State, cause error) (control.State, error) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), c.config.CleanupTimeout)
	defer cancel()
	rollbackErr := c.rollback(cleanupCtx, transitionID, previous)
	final := state
	final.Phase = control.PhaseReconciling
	final.Health = control.HealthError
	final.Admission = control.AdmissionClosed
	final.ActiveWorkload = control.WorkloadUnknown
	if rollbackErr == nil {
		final.ActiveWorkload = previous.ActiveWorkload
	}
	_ = c.store.AppendTransitionEvent(cleanupCtx, store.TransitionEvent{
		TransitionID: transitionID, Phase: final.Phase, Kind: "observation",
		Action: "transition failed", Outcome: failureCode(cause),
	})
	updated, finishErr := c.store.FinishTransition(cleanupCtx, transitionID, "failed", state.Version, final)
	return updated, errors.Join(cause, rollbackErr, finishErr)
}

func (c *Controller) rollback(ctx context.Context, transitionID string, previous control.State) error {
	snapshot, err := c.runtime.Observe(ctx)
	if err != nil {
		return err
	}
	switch previous.ActiveWorkload {
	case control.WorkloadText:
		if !snapshot.TextActive {
			if err := c.effect(ctx, transitionID, control.PhaseReconciling, "rollback start text", func() error {
				return c.runtime.Start(ctx, control.WorkloadText)
			}); err != nil {
				return err
			}
		}
		return c.waitReady(ctx, control.WorkloadText, time.Now().Add(c.config.CleanupTimeout))
	case control.WorkloadMedia:
		if snapshot.TextActive {
			if err := c.effect(ctx, transitionID, control.PhaseReconciling, "rollback stop text", func() error {
				return c.runtime.Stop(ctx, control.WorkloadText)
			}); err != nil {
				return err
			}
		}
		if !snapshot.MediaReady {
			if err := c.effect(ctx, transitionID, control.PhaseReconciling, "rollback start media", func() error {
				return c.runtime.Start(ctx, control.WorkloadMedia)
			}); err != nil {
				return err
			}
		}
		return c.waitReady(ctx, control.WorkloadMedia, time.Now().Add(c.config.CleanupTimeout))
	default:
		if snapshot.TextActive {
			return c.effect(ctx, transitionID, control.PhaseReconciling, "rollback stop text", func() error {
				return c.runtime.Stop(ctx, control.WorkloadText)
			})
		}
		return nil
	}
}

func (c *Controller) latchObservationFailure(ctx context.Context, state control.State, cause error) (control.State, error) {
	state.ActiveWorkload = control.WorkloadUnknown
	state.Phase = control.PhaseReconciling
	state.Health = control.HealthError
	state.Admission = control.AdmissionClosed
	updated, err := c.store.UpdateState(ctx, state.Version, state)
	return updated, errors.Join(cause, err)
}

func failureCode(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, ErrDrainTimeout), errors.Is(err, ErrVerifyTimeout):
		return "timeout"
	default:
		return "failed"
	}
}
