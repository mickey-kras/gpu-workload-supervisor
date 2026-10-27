package supervisor

import (
	"context"
	"errors"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

type trainingStore interface {
	PreflightTraining(context.Context, string, control.Workload, string) error
	TrainingJob(context.Context, string) (control.TrainingJob, error)
	ClaimTraining(context.Context, string, control.Workload, string) (control.TrainingJob, error)
	ReserveTraining(context.Context, control.TrainingJob) (control.TrainingJob, error)
	AbortTrainingPreparation(context.Context, control.TrainingJob) (control.State, error)
	RecordTrainingLaunch(context.Context, control.TrainingJob, string) error
	RecordTrainingTerminal(context.Context, control.TrainingJob, control.TrainingTerminal) error
	RecordTrainingMemoryPeak(context.Context, control.TrainingJob, uint64) error
	FinishTraining(context.Context, control.TrainingJob, string) (control.State, error)
}

type trainingRuntime interface {
	StartTraining(context.Context, control.WorkloadProfile, control.TrainingJob) (string, error)
	TrainingTerminal(context.Context, control.WorkloadProfile, control.TrainingJob) (control.TrainingTerminal, error)
	ReleaseTraining(context.Context, control.WorkloadProfile) error
	TrainingMemoryPeak(context.Context, control.WorkloadProfile, control.TrainingJob) (uint64, error)
}

func (c *Controller) trainingCapabilities() (trainingStore, trainingRuntime, error) {
	s, ok := c.store.(trainingStore)
	r, supported := c.runtime.(trainingRuntime)
	if !ok || !supported {
		return nil, nil, errors.New("finite training is unsupported by this controller")
	}
	return s, r, nil
}

func (c *Controller) TrainingStatus(ctx context.Context, id string) (control.TrainingJob, error) {
	s, _, err := c.trainingCapabilities()
	if err != nil {
		return control.TrainingJob{}, err
	}
	return s.TrainingJob(ctx, id)
}

// StartTraining runs under the controller process gate, like Switch. Only the
// launch has a deadline; the durable admission covers the full finite execution.
func (c *Controller) StartTraining(ctx context.Context, id string, target control.Workload) (control.TrainingJob, error) {
	s, r, err := c.trainingCapabilities()
	if err != nil {
		return control.TrainingJob{}, err
	}
	if err := control.ValidateRequestID(id); err != nil {
		return control.TrainingJob{}, err
	}
	p, ok := c.config.Catalog.Catalog.Profile(target)
	if !ok || p.Trainer == nil {
		return control.TrainingJob{}, errors.New("target is not a trainer")
	}
	if err := s.PreflightTraining(ctx, id, target, c.config.Catalog.Revision); err != nil {
		return control.TrainingJob{}, err
	}
	// Qualify runtime bindings and output destinations before claiming identity
	// or closing admission for the GPU handoff.
	if err := c.runAction(ctx, c.runtime.Preflight); err != nil {
		return control.TrainingJob{}, err
	}
	claim, err := s.ClaimTraining(ctx, id, target, c.config.Catalog.Revision)
	if err != nil {
		return claim, err
	}
	if _, err := c.Switch(ctx, control.WorkloadIdle, "training-start"); err != nil {
		return claim, c.abortTrainingPreparation(ctx, s, claim, err)
	}
	job, err := s.ReserveTraining(ctx, claim)
	if err != nil {
		return job, c.abortTrainingPreparation(ctx, s, claim, err)
	}
	actionCtx, cancel := context.WithTimeout(ctx, c.config.ActionTimeout)
	invocation, startErr := r.StartTraining(actionCtx, p, job)
	cancel()
	if invocation == "" && startErr == nil {
		startErr = errors.New("trainer start returned no invocation evidence")
	}
	if invocation != "" {
		finalCtx, finish := context.WithTimeout(context.Background(), c.config.FinalizeTimeout)
		err = s.RecordTrainingLaunch(finalCtx, job, invocation)
		finish()
		if err == nil {
			job.InvocationID = invocation
			job.Status = "running"
		}
	}
	return job, errors.Join(startErr, err)
}

func (c *Controller) activeTraining(ctx context.Context, id string) (trainingStore, trainingRuntime, control.WorkloadProfile, control.TrainingJob, error) {
	s, r, err := c.trainingCapabilities()
	if err != nil {
		return s, r, control.WorkloadProfile{}, control.TrainingJob{}, err
	}
	job, err := s.TrainingJob(ctx, id)
	if err != nil {
		return s, r, control.WorkloadProfile{}, job, err
	}
	p, ok := c.config.Catalog.Catalog.Profile(job.Workload)
	if !ok || p.Trainer == nil || job.CatalogRevision != c.config.Catalog.Revision {
		return s, r, p, job, store.ErrVersionConflict
	}
	state, err := c.store.State(ctx)
	if err != nil {
		return s, r, p, job, err
	}
	if state.LeaseFence != job.Fence || control.TrainingTerminalOutcome(job.Status) {
		return s, r, p, job, store.ErrStaleFence
	}
	return s, r, p, job, nil
}

// ReconcileTraining never starts a process. Missing/ambiguous evidence keeps
// admission pending until explicit recovery stops and verifies the whole job.
func (c *Controller) ReconcileTraining(ctx context.Context, id string) (control.State, error) {
	s, r, p, job, err := c.activeTraining(ctx, id)
	if err != nil {
		return control.State{}, err
	}
	outcome, err := c.trainingOutcome(ctx, s, r, p, job)
	if err != nil {
		return control.State{}, err
	}
	return c.finishTraining(ctx, s, r, p, job, outcome)
}

func (c *Controller) trainingOutcome(ctx context.Context, s trainingStore, r trainingRuntime, p control.WorkloadProfile, job control.TrainingJob) (string, error) {
	if job.TerminalOutcome != "" {
		if !control.TrainingTerminalOutcome(job.TerminalOutcome) {
			return "", store.ErrStaleFence
		}
		return job.TerminalOutcome, nil
	}
	actionCtx, cancel := context.WithTimeout(ctx, c.config.ActionTimeout)
	evidence, err := r.TrainingTerminal(actionCtx, p, job)
	cancel()
	if err != nil {
		return "", err
	}
	if !evidence.Matches(job) {
		return "", store.ErrStaleFence
	}
	// No cleanup effect is allowed until the receipt is durably committed.
	if err := s.RecordTrainingTerminal(ctx, job, evidence); err != nil {
		return "", err
	}
	return evidence.Outcome, nil
}

// RecoverTraining explicitly authorizes stopping the configured trainer after
// a launch/controller crash. It never restarts or resumes training.
func (c *Controller) RecoverTraining(ctx context.Context, id string) (control.State, error) {
	if state, handled, err := c.recoverTrainingPreparation(ctx, id); handled || err != nil {
		return state, err
	}
	s, r, p, job, err := c.activeTraining(ctx, id)
	if err != nil {
		return control.State{}, err
	}
	outcome := job.TerminalOutcome
	if outcome == "" {
		outcome = "interrupted"
	}
	if !control.TrainingTerminalOutcome(outcome) {
		return control.State{}, store.ErrStaleFence
	}
	return c.finishTraining(ctx, s, r, p, job, outcome)
}

func (c *Controller) finishTraining(ctx context.Context, s trainingStore, r trainingRuntime, p control.WorkloadProfile, job control.TrainingJob, outcome string) (control.State, error) {
	c.recordTrainingMemoryPeak(ctx, s, r, p, job)
	if err := c.runAction(ctx, func(actionCtx context.Context) error { return c.runtime.Stop(actionCtx, p.ID) }); err != nil {
		return control.State{}, err
	}
	if err := c.pollUntil(ctx, c.now().Add(c.config.VerifyTimeout), func(probeCtx context.Context) error { return r.ReleaseTraining(probeCtx, p) }); err != nil {
		return control.State{}, err
	}
	finalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.config.FinalizeTimeout)
	defer cancel()
	return s.FinishTraining(finalCtx, job, outcome)
}

// trainingPeakTimeout bounds the peak read and, separately, the store write,
// so that the recording delays the stop by a short time at most. Tests lower it.
var trainingPeakTimeout = 5 * time.Second

// recordTrainingMemoryPeak stores the memory peak before the stop. It is best
// effort: a peak that cannot be read or stored never blocks the stop. The
// controller has no log, so an absent memoryPeakBytes is the only trace;
// qualification then uses external samples of the unit cgroup memory.peak.
// The runner peaks file is only an unverified hint.
func (c *Controller) recordTrainingMemoryPeak(ctx context.Context, s trainingStore, r trainingRuntime, p control.WorkloadProfile, job control.TrainingJob) {
	if job.InvocationID == "" || ctx.Err() != nil {
		return
	}
	// Neither step takes the cancellation of the caller, so the stop keeps its
	// own deadline. The write gets its own budget after the read returns, so a
	// slow read never loses a peak that it read.
	read, cancelRead := context.WithTimeout(context.WithoutCancel(ctx), min(c.config.ActionTimeout, trainingPeakTimeout))
	peak, err := r.TrainingMemoryPeak(read, p, job)
	cancelRead()
	if err != nil {
		return
	}
	write, cancelWrite := context.WithTimeout(context.WithoutCancel(ctx), min(c.config.FinalizeTimeout, trainingPeakTimeout))
	defer cancelWrite()
	_ = s.RecordTrainingMemoryPeak(write, job, peak)
}

func (c *Controller) abortTrainingPreparation(ctx context.Context, s trainingStore, claim control.TrainingJob, cause error) error {
	finalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.config.FinalizeTimeout)
	defer cancel()
	_, err := s.AbortTrainingPreparation(finalCtx, claim)
	return errors.Join(cause, err)
}

func (c *Controller) recoverTrainingPreparation(ctx context.Context, id string) (control.State, bool, error) {
	s, _, err := c.trainingCapabilities()
	if err != nil {
		return control.State{}, false, err
	}
	job, err := s.TrainingJob(ctx, id)
	if err != nil {
		return control.State{}, false, err
	}
	if job.Status != "preparing" {
		return control.State{}, false, nil
	}
	state, err := s.AbortTrainingPreparation(ctx, job)
	return state, true, err
}
