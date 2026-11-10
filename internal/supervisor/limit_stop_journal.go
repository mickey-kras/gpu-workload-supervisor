package supervisor

import (
	"context"
	"errors"
	"fmt"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

// journaledLimitStop stops a unit with limits as a journaled transition
// from the latched state. The journal records the reason, the stop through
// the system manager and the release check. A passed release check commits
// idle; otherwise the transition fails with an unknown workload. Either
// way the error state stays until recover.
//
// Once the transition starts, each step runs on a context that the caller
// cannot cancel, bounded by its own timeout, so the transition always ends.
// stopped tells that the stop through the system manager passed.
func (c *Controller) journaledLimitStop(ctx context.Context, state control.State, p control.WorkloadProfile, reason string) (stopped bool, err error) {
	transitionID, err := c.id()
	if err != nil {
		return false, err
	}
	target := limitStopTarget(state)
	transition := store.Transition{
		ID: transitionID, ConfigurationRevision: c.config.Catalog.Revision,
		Source: state, Target: target, Previous: state, Initiator: limitStopInitiator,
		Phase: control.PhaseDraining, Deadline: c.now(),
	}
	started, err := c.store.StartTransition(ctx, state.Version, transition)
	if err != nil {
		return false, fmt.Errorf("start the limit stop of %s: %w", p.ID, err)
	}
	work := context.WithoutCancel(ctx)
	state, stopped, err = c.runLimitStop(work, transitionID, started, p, reason)
	return stopped, c.finishLimitStop(work, transitionID, state, err)
}

// limitStopTarget is the latched Idle state of a limit stop.
func limitStopTarget(state control.State) control.State {
	target := closedReconciling(state)
	target.DesiredWorkload, target.ActiveWorkload = control.WorkloadIdle, control.WorkloadIdle
	return target
}

// runLimitStop records the reason, stops the unit and checks the release.
// It returns the last state that it wrote, and whether the stop passed.
func (c *Controller) runLimitStop(ctx context.Context, transitionID string, state control.State, p control.WorkloadProfile, reason string) (control.State, bool, error) {
	if err := c.journalLimitStop(ctx, transitionID, state.Phase, "limit stop reason "+string(p.ID), reason); err != nil {
		return state, false, err
	}
	unloading, err := c.setLimitStopPhase(ctx, transitionID, state.Version, control.PhaseUnloading)
	if err != nil {
		return state, false, err
	}
	// effect bounds the stop by ActionTimeout. The context adds
	// FinalizeTimeout, so the observation of a stop that used its whole
	// action timeout still reaches the journal.
	stopCtx, cancel := context.WithTimeout(ctx, c.config.ActionTimeout+c.config.FinalizeTimeout)
	err = c.effect(stopCtx, transitionID, unloading.Phase, "limit stop "+string(p.ID), func(ctx context.Context) error {
		return c.runtime.Stop(ctx, p.ID)
	})
	cancel()
	if err != nil {
		return unloading, false, fmt.Errorf("limit stop of %s: %w", p.ID, err)
	}
	verifying, err := c.setLimitStopPhase(ctx, transitionID, unloading.Version, control.PhaseVerifying)
	if err != nil {
		return unloading, true, err
	}
	releaseErr := c.limitStopRelease(ctx, p.ID)
	outcome := "ok"
	if releaseErr != nil {
		outcome = failureCode(releaseErr)
	}
	journalErr := c.journalLimitStop(ctx, transitionID, verifying.Phase, "limit stop release check "+string(p.ID), outcome)
	return verifying, true, errors.Join(releaseErr, journalErr)
}

// setLimitStopPhase writes the phase of the stop transition within
// FinalizeTimeout.
func (c *Controller) setLimitStopPhase(ctx context.Context, transitionID string, version uint64, phase control.Phase) (control.State, error) {
	finalizeCtx, cancel := context.WithTimeout(ctx, c.config.FinalizeTimeout)
	defer cancel()
	return c.store.SetTransitionPhase(finalizeCtx, transitionID, version, phase)
}

// journalLimitStop appends one observation to the journal of the stop.
func (c *Controller) journalLimitStop(ctx context.Context, transitionID string, phase control.Phase, action, outcome string) error {
	finalizeCtx, cancel := context.WithTimeout(ctx, c.config.FinalizeTimeout)
	defer cancel()
	return c.store.AppendTransitionEvent(finalizeCtx, store.TransitionEvent{
		TransitionID: transitionID, Phase: phase, Kind: "observation", Action: action, Outcome: outcome,
	})
}

// limitStopRelease checks the release after the stop. The stop and the
// outcome of the limits are no evidence. Every unit must be stopped with
// an empty recursive cgroup, the GPU must have no compute process, and
// the stopped unit must have its capacity. A failed binding check of the
// release ends the poll at once: it stays failed until the binding is
// restored, so a poll would only hold the gate on each tick.
func (c *Controller) limitStopRelease(ctx context.Context, id control.Workload) error {
	probe := func(ctx context.Context) error {
		err := c.probeLimitStopRelease(ctx, id)
		if gpuruntime.DefiniteBindingFailure(err) {
			// pollUntil ends at once on this class.
			return errors.Join(gpuruntime.ErrLimitCheckFailed, err)
		}
		return err
	}
	err := c.pollUntil(ctx, c.now().Add(c.config.VerifyTimeout), probe)
	if gpuruntime.DefiniteBindingFailure(err) {
		return fmt.Errorf("release check after the limit stop of %s: the launch binding changed; restore it, then run recover: %w", id, err)
	}
	if err != nil {
		return fmt.Errorf("release check after the limit stop of %s: %w", id, err)
	}
	return nil
}

// probeLimitStopRelease runs the release check of a limit stop once.
func (c *Controller) probeLimitStopRelease(ctx context.Context, id control.Workload) error {
	probeCtx, cancel := context.WithTimeout(ctx, c.config.ActionTimeout)
	defer cancel()
	return c.runtime.LimitStopReleased(probeCtx, id)
}

// finishLimitStop ends the transition of a limit stop. It commits idle
// when cause is nil. Otherwise the transition fails, the workload is
// unknown, and the next tick tries the stop again. The record stays in
// both cases until a state write ends the latch.
func (c *Controller) finishLimitStop(ctx context.Context, transitionID string, state control.State, cause error) error {
	final := limitStopTarget(state)
	status := "committed"
	if cause != nil {
		final.ActiveWorkload, status = control.WorkloadUnknown, "failed"
	}
	finalizeCtx, cancel := context.WithTimeout(ctx, c.config.FinalizeTimeout)
	defer cancel()
	_, err := c.store.FinishTransition(finalizeCtx, transitionID, status, state.Version, final)
	return errors.Join(cause, err)
}
