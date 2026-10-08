package supervisor

import (
	"context"
	"errors"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

const temporaryStoreUnavailable = "temporary discovery store unavailable"

type temporaryDiscoveryStore interface {
	RecordTemporaryLaunch(context.Context, string, string, control.TemporaryDiscoveryLaunchEvidence) error
	StartTemporaryDiscovery(context.Context, control.OperatorPrecondition, store.Transition, control.TemporaryDiscoverySession) (control.State, error)
	TemporaryDiscoveryStatus(context.Context) (*control.TemporaryDiscoverySession, error)
	CheckTemporaryDiscovery(context.Context, string, string) error
	UpdateTemporaryDiscovery(context.Context, string, string, string, string, string) error
	FinishTemporaryDiscovery(context.Context, string, string) (control.State, error)
}
type temporaryDiscoveryRuntime interface {
	PrepareTemporaryDiscovery(context.Context, control.TemporaryDiscoveryCandidate) error
	StartTemporaryDiscovery(context.Context, control.TemporaryDiscoveryCandidate) (control.TemporaryDiscoveryLaunchEvidence, error)
	StopTemporaryDiscovery(context.Context, control.TemporaryDiscoveryCandidate, string) error
}

// DiscoverNativeTemporary retains the closed admission generation until exact
// invocation cleanup and recursive cgroup release have been verified. Inventory
// results are returned only after cleanup commits. Caller cancellation cannot
// cancel the separately bounded cleanup phase.
func (c *Controller) DiscoverNativeTemporary(ctx context.Context, v control.TemporaryDiscoveryCandidate, e control.OperatorPrecondition, consent bool, inspect func(context.Context, string) ([]byte, error)) ([]byte, error) {
	if !consent || inspect == nil {
		return nil, errors.New("explicit temporary-start consent is required")
	}
	s, ok := c.store.(temporaryDiscoveryStore)
	if !ok {
		return nil, errors.New(temporaryStoreUnavailable)
	}
	r, ok := c.runtime.(temporaryDiscoveryRuntime)
	if !ok {
		return nil, errors.New("temporary discovery runtime unavailable")
	}
	if err := c.checkCatalog(ctx); err != nil {
		return nil, err
	}
	if err := c.store.CheckActivationPrecondition(ctx, e); err != nil {
		return nil, err
	}
	gate, err := c.store.AcquireUserExecution(ctx, false)
	if err != nil {
		return nil, err
	}
	defer gate.Close()
	if err = c.preflight(ctx); err != nil {
		return nil, err
	}
	if err = r.PrepareTemporaryDiscovery(ctx, v); err != nil {
		return nil, err
	}
	id, token, err := c.beginTemporaryDiscovery(ctx, s, v, e)
	if err != nil {
		return nil, err
	}
	var result []byte
	startCtx, cancelStart := context.WithTimeout(ctx, c.config.ActionTimeout)
	launch, startErr := r.StartTemporaryDiscovery(startCtx, v)
	cancelStart()
	startErr = c.recordTemporaryStart(s, id, token, launch, startErr)
	if startErr == nil {
		result, err = inspect(ctx, v.Endpoint)
	} else {
		err = startErr
	}
	cleanupErr := c.cleanupTemporaryDiscovery(context.Background(), id, token)
	if err != nil || cleanupErr != nil || ctx.Err() != nil {
		return nil, errors.Join(err, cleanupErr, ctx.Err())
	}
	return result, nil
}

func (c *Controller) beginTemporaryDiscovery(ctx context.Context, s temporaryDiscoveryStore, v control.TemporaryDiscoveryCandidate, e control.OperatorPrecondition) (string, string, error) {
	current, err := c.store.State(ctx)
	if err != nil {
		return "", "", err
	}
	id, err := c.id()
	if err != nil {
		return "", "", err
	}
	token, err := c.id()
	if err != nil {
		return "", "", err
	}
	tr := store.Transition{ID: id, Source: current, Target: current, Previous: current, Initiator: "temporary-native-discovery", Phase: control.PhaseDraining, Deadline: c.now().Add(c.config.ActionTimeout), ConfigurationRevision: e.ConfigurationRevision}
	record := control.TemporaryDiscoverySession{ID: id, Token: token, Candidate: v, PriorStopped: true, Status: "starting"}
	if _, err = s.StartTemporaryDiscovery(ctx, e, tr, record); err != nil {
		return "", "", err
	}
	return id, token, nil
}

func (c *Controller) recordTemporaryStart(s temporaryDiscoveryStore, id, token string, launch control.TemporaryDiscoveryLaunchEvidence, startErr error) error {
	recordCtx, cancelRecord := context.WithTimeout(context.Background(), c.config.FinalizeTimeout)
	persistLaunchErr := s.RecordTemporaryLaunch(recordCtx, id, token, launch)
	cancelRecord()
	if persistLaunchErr != nil {
		startErr = errors.Join(startErr, persistLaunchErr)
	}
	status := "running"
	if startErr != nil {
		status = "cleanup_required"
	}
	updateCtx, cancelUpdate := context.WithTimeout(context.Background(), c.config.FinalizeTimeout)
	persistErr := s.UpdateTemporaryDiscovery(updateCtx, id, token, launch.InvocationID, status, failureMessage(startErr))
	cancelUpdate()
	return errors.Join(startErr, persistErr)
}

func failureMessage(err error) string {
	if err != nil {
		return "temporary discovery failed; inspect service and explicit cleanup status"
	}
	return ""
}

func cleanupFailureMessage(invocation string) string {
	if invocation == "" {
		return "Cleanup failed. Admission remains closed. No service invocation was recorded; inspect the service manually before retrying explicit cleanup."
	}
	return "Cleanup failed. Admission remains closed. Inspect the recorded service invocation before retrying explicit cleanup."
}

func (c *Controller) CleanupTemporaryDiscovery(ctx context.Context, id, token string) error {
	gate, err := c.store.AcquireUserExecution(ctx, false)
	if err != nil {
		return err
	}
	defer gate.Close()
	return c.cleanupTemporaryDiscovery(ctx, id, token)
}
func (c *Controller) cleanupTemporaryDiscovery(ctx context.Context, id, token string) error {
	s, ok := c.store.(temporaryDiscoveryStore)
	if !ok {
		return errors.New(temporaryStoreUnavailable)
	}
	r, ok := c.runtime.(temporaryDiscoveryRuntime)
	if !ok {
		return errors.New("temporary discovery runtime unavailable")
	}
	cleanupCtx, cancel := context.WithTimeout(ctx, c.config.CleanupTimeout)
	defer cancel()
	if err := s.CheckTemporaryDiscovery(cleanupCtx, id, token); err != nil {
		return err
	}
	record, err := s.TemporaryDiscoveryStatus(cleanupCtx)
	if err != nil {
		return err
	}
	if record == nil || record.ID != id {
		return store.ErrStaleFence
	}
	invocation := record.InvocationID
	if invocation == "" {
		invocation = record.LaunchEvidence.InvocationID
	}
	err = r.StopTemporaryDiscovery(cleanupCtx, record.Candidate, invocation)
	if err == nil {
		_, err = s.FinishTemporaryDiscovery(cleanupCtx, id, token)
	}
	if err != nil {
		finalCtx, cancel := context.WithTimeout(context.Background(), c.config.FinalizeTimeout)
		defer cancel()
		markErr := s.UpdateTemporaryDiscovery(finalCtx, id, token, invocation, "cleanup_required", cleanupFailureMessage(invocation))
		return errors.Join(store.ErrTemporaryCleanupRequired, err, markErr)
	}
	return nil
}

// TemporaryDiscoveryEligibility uses durable state only. The runtime is fully
// re-attested immediately before any lifecycle effect.
func (c *Controller) TemporaryDiscoveryEligibility(ctx context.Context) (control.OperatorPrecondition, error) {
	state, err := c.store.State(ctx)
	if err != nil {
		return control.OperatorPrecondition{}, err
	}
	e := control.OperatorPrecondition{Incarnation: state.LeaseFence.Incarnation, Version: state.Version, Owner: state.Owner, ConfigurationRevision: c.config.Catalog.Revision}
	return e, c.store.CheckActivationPrecondition(ctx, e)
}

func (c *Controller) TemporaryDiscoveryStatus(ctx context.Context) (*control.TemporaryDiscoverySession, error) {
	s, ok := c.store.(temporaryDiscoveryStore)
	if !ok {
		return nil, errors.New(temporaryStoreUnavailable)
	}
	return s.TemporaryDiscoveryStatus(ctx)
}
