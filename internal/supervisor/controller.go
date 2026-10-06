package supervisor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

var (
	ErrSupervisorOwned    = errors.New("GPU is in supervisor control mode")
	ErrUserOwned          = errors.New("GPU is in user control mode")
	ErrTransitionRunning  = errors.New("another transition is running")
	ErrDrainTimeout       = errors.New("timed out waiting for admitted work")
	ErrVerifyTimeout      = errors.New("timed out verifying workload")
	ErrRecoveryRequired   = errors.New("explicit recovery is required")
	ErrReconcileRequired  = errors.New("reconciliation required before switching")
	ErrInvariant          = errors.New("observed runtime violates control state")
	ErrRuntimeObservation = errors.New("runtime observation failed")
	ErrStateVerification  = errors.New("runtime state verification failed")
	ErrHealthCheck        = errors.New("runtime health check failed")
)

type storeGateway interface {
	AcquireUserExecution(context.Context, bool) (*lock.File, error)
	State(context.Context) (control.State, error)
	Catalog(context.Context) (control.CatalogSnapshot, error)
	UpdateState(context.Context, uint64, control.State) (control.State, error)
	StartTransition(context.Context, uint64, store.Transition) (control.State, error)
	StartOperatorTransition(context.Context, control.OperatorPrecondition, store.Transition) (control.State, error)
	SetTransitionPhase(context.Context, string, uint64, control.Phase) (control.State, error)
	FinishTransition(context.Context, string, string, uint64, control.State) (control.State, error)
	AppendTransitionEvent(context.Context, store.TransitionEvent) error
	PendingTransitionWork(context.Context, string) (int, error)
	PendingWork(context.Context) (int, error)
	PendingWorkExcept(context.Context, control.Workload) (int, error)
	InProgressTransition(context.Context) (string, error)
	Recover(context.Context, uint64, control.State, string) (control.State, error)
	RotateFenceAndCloseAdmission(context.Context, uint64) (control.State, error)
	ResolveUnfinishedWork(context.Context, uint64, string) (int64, error)
}

type Config struct {
	Catalog         *control.CatalogSnapshot
	DrainTimeout    time.Duration
	VerifyTimeout   time.Duration
	ActionTimeout   time.Duration
	CleanupTimeout  time.Duration
	FinalizeTimeout time.Duration
	PollInterval    time.Duration
}

type Controller struct {
	store   storeGateway
	runtime gpuruntime.Manager
	config  Config
	now     func() time.Time
	id      func() (string, error)
}

func New(stateStore *store.Store, runtime gpuruntime.Manager, config Config) (*Controller, error) {
	return newController(stateStore, runtime, config, time.Now, control.NewUUID)
}

func newController(stateStore storeGateway, runtime gpuruntime.Manager, config Config, now func() time.Time, id func() (string, error)) (*Controller, error) {
	if stateStore == nil || runtime == nil {
		return nil, errors.New("store and runtime are required")
	}
	if config.DrainTimeout <= 0 || config.VerifyTimeout <= 0 || config.ActionTimeout <= 0 || config.CleanupTimeout <= 0 || config.FinalizeTimeout <= 0 || config.PollInterval <= 0 {
		return nil, errors.New("timeouts and poll interval must be greater than zero")
	}
	if config.Catalog == nil {
		return nil, errors.New("workload catalog is required")
	}
	snapshot := *config.Catalog
	snapshot.Catalog = snapshot.Catalog.Clone()
	if err := snapshot.Catalog.Validate(); err != nil {
		return nil, err
	}
	config.Catalog = &snapshot
	return &Controller{store: stateStore, runtime: runtime, config: config, now: now, id: id}, nil
}

func (c *Controller) Reconcile(ctx context.Context) (control.State, error) {
	return c.reconcileCatalog(ctx, false)
}

func (c *Controller) enterReconciliation(ctx context.Context, state control.State) (control.State, error) {
	closed := closedReconciling(state)
	closed.ActiveWorkload = control.WorkloadUnknown
	entryCtx, cancel := context.WithTimeout(context.Background(), c.config.FinalizeTimeout)
	entered, err := c.store.Recover(entryCtx, state.Version, closed, "reconciliation-entry")
	cancel()
	if err != nil {
		return state, err
	}
	return entered, ctx.Err()
}

func (c *Controller) beginRecovery(ctx context.Context) (control.State, error) {
	if err := c.checkCatalog(ctx); err != nil {
		return control.State{}, err
	}
	state, err := c.store.State(ctx)
	if err != nil {
		return control.State{}, err
	}
	if state.Owner == control.OwnerUser {
		return state, ErrUserOwned
	}
	// Commit the safety state before observing or stopping runtimes. This
	// bounded entry is independent of caller cancellation, and leaves failures
	// durably closed without relying on a later cleanup write.
	closed := closedReconciling(state)
	closed.ActiveWorkload = control.WorkloadUnknown
	entryCtx, cancelEntry := context.WithTimeout(context.Background(), c.config.FinalizeTimeout)
	entered, err := c.store.Recover(entryCtx, state.Version, closed, "operator-recovery-entry")
	cancelEntry()
	if err != nil {
		return state, err
	}
	state = entered
	if err := ctx.Err(); err != nil {
		return state, err
	}
	if err := c.preflight(ctx); err != nil {
		return state, err
	}
	return state, nil
}

func (c *Controller) Recover(ctx context.Context) (control.State, error) {
	return c.reconcileCatalog(ctx, true)
}

// ResolveUnfinishedWork is an explicit, disruptive operator action. The caller
// must hold the exclusive proxy lifetime lock until this method returns, so no
// admitted request can be forwarded after runtime shutdown. Recovery remains
// separate: this operation never reopens admission.
func (c *Controller) ResolveUnfinishedWork(ctx context.Context, reason string) (control.State, int64, error) {
	if err := c.checkCatalog(ctx); err != nil {
		return control.State{}, 0, err
	}
	reason = strings.TrimSpace(reason)
	if len(reason) == 0 || len(reason) > 512 {
		return control.State{}, 0, errors.New("resolution reason must contain 1 to 512 bytes")
	}
	state, err := c.store.State(ctx)
	if err != nil {
		return control.State{}, 0, err
	}
	if state.Owner == control.OwnerUser {
		return state, 0, ErrUserOwned
	}
	state, err = c.store.RotateFenceAndCloseAdmission(ctx, state.Version)
	if err != nil {
		return state, 0, err
	}
	if err := c.preflight(ctx); err != nil {
		return state, 0, err
	}
	if err := c.runAction(ctx, c.runtime.StopForRecovery); err != nil {
		return state, 0, fmt.Errorf("stop runtimes before work resolution: %w", err)
	}
	if err := c.waitReleased(ctx, c.now().Add(c.config.VerifyTimeout)); err != nil {
		return state, 0, fmt.Errorf("verify release before work resolution: %w", err)
	}
	snapshot, err := c.observe(ctx)
	if err != nil {
		return state, 0, fmt.Errorf("observe stopped runtimes: %w", err)
	}
	if snapshot.AnyActive() {
		return state, 0, fmt.Errorf("%w: runtime remains active after stop", ErrStateVerification)
	}
	count, err := c.store.ResolveUnfinishedWork(ctx, state.Version, reason)
	return state, count, err
}

func (c *Controller) waitForDrain(ctx context.Context, transitionID string, deadline time.Time) error {
	return c.waitForWork(ctx, deadline, func(ctx context.Context) (int, error) {
		return c.store.PendingTransitionWork(ctx, transitionID)
	})
}

func (c *Controller) waitForWork(ctx context.Context, deadline time.Time, pendingWork func(context.Context) (int, error)) error {
	drainCtx, cancel := context.WithTimeout(ctx, deadline.Sub(c.now()))
	defer cancel()
	ticker := time.NewTicker(c.config.PollInterval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if c.pastDeadline(drainCtx, deadline) {
			return ErrDrainTimeout
		}
		pending, err := pendingWork(drainCtx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if c.pastDeadline(drainCtx, deadline) {
			return ErrDrainTimeout
		}
		if err != nil {
			return err
		}
		if pending == 0 {
			return nil
		}
		select {
		case <-drainCtx.Done():
		case <-ticker.C:
		}
	}
}

func (c *Controller) pastDeadline(ctx context.Context, deadline time.Time) bool {
	return ctx.Err() != nil || !c.now().Before(deadline)
}

func (c *Controller) waitReady(ctx context.Context, target control.Workload, deadline time.Time) error {
	return c.pollUntil(ctx, deadline, func(probeCtx context.Context) error {
		return c.checkReady(probeCtx, target)
	})
}

// pollUntil bounds the entire verification phase, including observations and
// health commands, rather than checking the budget only between probes.
func (c *Controller) pollUntil(ctx context.Context, deadline time.Time, probe func(context.Context) error) error {
	verifyCtx, cancel := context.WithTimeout(ctx, deadline.Sub(c.now()))
	defer cancel()
	ticker := time.NewTicker(c.config.PollInterval)
	defer ticker.Stop()
	var lastErr error
	for {
		lastErr = probe(verifyCtx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if verifyCtx.Err() != nil || !c.now().Before(deadline) {
			return errors.Join(ErrVerifyTimeout, lastErr)
		}
		if lastErr == nil || errors.Is(lastErr, gpuruntime.ErrUnloadUnverified) {
			return lastErr
		}
		select {
		case <-verifyCtx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.Join(ErrVerifyTimeout, lastErr)
		case <-ticker.C:
		}
	}
}

func (c *Controller) checkReady(ctx context.Context, target control.Workload) error {
	snapshot, err := c.observe(ctx)
	if err != nil {
		return gpuruntime.SafeError(ErrRuntimeObservation.Error(), ErrRuntimeObservation, err)
	}
	if err := verifySnapshot(target, snapshot); err != nil {
		return err
	}
	if target == control.WorkloadIdle {
		if err := c.released(ctx); err != nil {
			return err
		}
	}
	if err := c.healthy(ctx, target); err != nil {
		return gpuruntime.SafeError(ErrHealthCheck.Error(), ErrHealthCheck, err)
	}
	return nil
}

func (c *Controller) setPhase(ctx context.Context, transitionID string, state control.State, phase control.Phase) (control.State, error) {
	next, err := c.store.SetTransitionPhase(ctx, transitionID, state.Version, phase)
	if err != nil {
		return state, err
	}
	return next, nil
}

func (c *Controller) observe(ctx context.Context) (gpuruntime.Snapshot, error) {
	probeCtx, cancel := context.WithTimeout(ctx, c.config.ActionTimeout)
	defer cancel()
	return c.runtime.Observe(probeCtx)
}

func (c *Controller) healthy(ctx context.Context, target control.Workload) error {
	probeCtx, cancel := context.WithTimeout(ctx, c.config.ActionTimeout)
	defer cancel()
	return c.runtime.Healthy(probeCtx, target)
}

func (c *Controller) released(ctx context.Context) error {
	return c.releasedFor(ctx, control.WorkloadIdle)
}

func (c *Controller) releasedFor(ctx context.Context, target control.Workload) error {
	probeCtx, cancel := context.WithTimeout(ctx, c.config.ActionTimeout)
	defer cancel()
	return c.runtime.ReleasedFor(probeCtx, target)
}

func (c *Controller) runAction(ctx context.Context, fn func(context.Context) error) error {
	actionCtx, cancel := context.WithTimeout(ctx, c.config.ActionTimeout)
	defer cancel()
	return fn(actionCtx)
}

func (c *Controller) effect(ctx context.Context, transitionID string, phase control.Phase, action string, fn func(context.Context) error) error {
	if err := c.store.AppendTransitionEvent(ctx, store.TransitionEvent{
		TransitionID: transitionID, Phase: phase, Kind: "intent", Action: action,
	}); err != nil {
		return err
	}
	actionCtx, cancel := context.WithTimeout(ctx, c.config.ActionTimeout)
	err := fn(actionCtx)
	cancel()
	outcome := "ok"
	if err != nil {
		outcome = failureCode(err)
	}
	journalErr := c.store.AppendTransitionEvent(ctx, store.TransitionEvent{
		TransitionID: transitionID, Phase: phase, Kind: "observation", Action: action, Outcome: outcome,
	})
	return errors.Join(err, journalErr)
}

func (c *Controller) waitReleased(ctx context.Context, deadline time.Time) error {
	return c.waitReleasedFor(ctx, control.WorkloadIdle, deadline)
}

func (c *Controller) fail(transitionID string, state, previous control.State, cause error) (control.State, error) {
	var rollbackErr error
	if previous.Owner == control.OwnerSupervisor {
		rollbackCtx, cancelRollback := context.WithTimeout(context.Background(), c.config.CleanupTimeout)
		rollbackErr = c.rollbackCatalog(rollbackCtx, transitionID, previous)
		cancelRollback()
	}

	final := closedReconciling(state)
	final.ActiveWorkload = control.WorkloadUnknown
	if rollbackErr == nil && previous.Owner == control.OwnerSupervisor {
		final.ActiveWorkload = previous.ActiveWorkload
	}

	finalizeCtx, cancelFinalize := context.WithTimeout(context.Background(), c.config.FinalizeTimeout)
	defer cancelFinalize()
	journalErr := c.store.AppendTransitionEvent(finalizeCtx, store.TransitionEvent{
		TransitionID: transitionID, Phase: final.Phase, Kind: "observation",
		Action: "transition failed", Outcome: failureCode(cause),
	})
	updated, finishErr := c.store.FinishTransition(finalizeCtx, transitionID, "failed", state.Version, final)
	return updated, errors.Join(cause, rollbackErr, journalErr, finishErr)
}

func (c *Controller) latchObservationFailure(ctx context.Context, state control.State, cause error) (control.State, error) {
	state = closedReconciling(state)
	state.ActiveWorkload = control.WorkloadUnknown
	finalizeCtx, cancel := context.WithTimeout(context.Background(), c.config.FinalizeTimeout)
	defer cancel()
	updated, err := c.store.UpdateState(finalizeCtx, state.Version, state)
	return updated, errors.Join(cause, err)
}

func closedReconciling(s control.State) control.State {
	s.Phase = control.PhaseReconciling
	s.Health = control.HealthError
	s.Admission = control.AdmissionClosed
	return s
}

func failureCode(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, ErrDrainTimeout), errors.Is(err, ErrVerifyTimeout):
		return "timeout"
	case errors.Is(err, gpuruntime.ErrCapacity):
		return "capacity"
	case errors.Is(err, ErrRuntimeObservation):
		return "runtime-observation"
	case errors.Is(err, ErrStateVerification), errors.Is(err, ErrInvariant):
		return "state-mismatch"
	case errors.Is(err, ErrHealthCheck):
		return "health-check"
	default:
		return "failed"
	}
}
