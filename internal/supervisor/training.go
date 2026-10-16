package supervisor

import (
	"context"
	"database/sql"
	"errors"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

type trainingStore interface {
	TrainingJob(context.Context, string) (control.TrainingJob, error)
	ReserveTraining(context.Context, string, control.Workload, string) (control.TrainingJob, error)
	RecordTrainingLaunch(context.Context, control.TrainingJob, string) error
	FinishTraining(context.Context, control.TrainingJob, string) (control.State, error)
}

type trainingRuntime interface {
	StartTraining(context.Context, control.WorkloadProfile, control.TrainingJob) (string, error)
	TrainingTerminal(context.Context, control.WorkloadProfile, control.TrainingJob) (control.TrainingTerminal, error)
	ReleaseTraining(context.Context, control.WorkloadProfile) error
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
	if _, err := s.TrainingJob(ctx, id); !errors.Is(err, sql.ErrNoRows) {
		if err == nil {
			err = store.ErrRequestConflict
		}
		return control.TrainingJob{}, err
	}
	if _, err := c.Switch(ctx, control.WorkloadIdle, "training-start"); err != nil {
		return control.TrainingJob{}, err
	}
	job, err := s.ReserveTraining(ctx, id, target, c.config.Catalog.Revision)
	if err != nil {
		return job, err
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
	actionCtx, cancel := context.WithTimeout(ctx, c.config.ActionTimeout)
	evidence, err := r.TrainingTerminal(actionCtx, p, job)
	cancel()
	if err != nil {
		return control.State{}, err
	}
	if !evidence.Matches(job) {
		return control.State{}, store.ErrStaleFence
	}
	return c.finishTraining(ctx, s, r, p, job, evidence.Outcome)
}

// RecoverTraining explicitly authorizes stopping the configured trainer after
// a launch/controller crash. It never restarts or resumes training.
func (c *Controller) RecoverTraining(ctx context.Context, id string) (control.State, error) {
	s, r, p, job, err := c.activeTraining(ctx, id)
	if err != nil {
		return control.State{}, err
	}
	return c.finishTraining(ctx, s, r, p, job, "interrupted")
}

func (c *Controller) finishTraining(ctx context.Context, s trainingStore, r trainingRuntime, p control.WorkloadProfile, job control.TrainingJob, outcome string) (control.State, error) {
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
