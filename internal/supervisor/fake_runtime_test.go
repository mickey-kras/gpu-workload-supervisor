package supervisor

import (
	"context"
	"errors"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

// The fakes have no failed units, so their recovery observation is their
// strict observation. Go has no virtual dispatch through embedding, so each
// fake that overrides Observe also overrides ObserveForRecovery.
func (r *fakeRuntime) ObserveForRecovery(ctx context.Context) (gpuruntime.Snapshot, error) {
	return r.Observe(ctx)
}

func (r observationFailure) ObserveForRecovery(ctx context.Context) (gpuruntime.Snapshot, error) {
	return r.Observe(ctx)
}

func (r *activationRuntime) ObserveForRecovery(ctx context.Context) (gpuruntime.Snapshot, error) {
	return r.Observe(ctx)
}

func (r *acceptanceRuntime) ObserveForRecovery(ctx context.Context) (gpuruntime.Snapshot, error) {
	return r.Observe(ctx)
}

func (r *acceptanceObservationRuntime) ObserveForRecovery(ctx context.Context) (gpuruntime.Snapshot, error) {
	return r.Observe(ctx)
}

func (r *catalogRuntime) ObserveForRecovery(ctx context.Context) (gpuruntime.Snapshot, error) {
	return r.Observe(ctx)
}

func (r *deadlineRuntime) ObserveForRecovery(ctx context.Context) (gpuruntime.Snapshot, error) {
	return r.Observe(ctx)
}

func (r *blockedHealthRuntime) ObserveForRecovery(ctx context.Context) (gpuruntime.Snapshot, error) {
	return r.Observe(ctx)
}

func (r *exclusiveRuntime) ObserveForRecovery(ctx context.Context) (gpuruntime.Snapshot, error) {
	return r.Observe(ctx)
}

func (r *cancelingObserveRuntime) ObserveForRecovery(ctx context.Context) (gpuruntime.Snapshot, error) {
	return r.Observe(ctx)
}

func (r *recoverySafetyRuntime) ObserveForRecovery(ctx context.Context) (gpuruntime.Snapshot, error) {
	return r.Observe(ctx)
}

func (r *releaseOrderedRuntime) ObserveForRecovery(ctx context.Context) (gpuruntime.Snapshot, error) {
	return r.Observe(ctx)
}

func (r *limitedRuntime) ObserveForRecovery(ctx context.Context) (gpuruntime.Snapshot, error) {
	return r.Observe(ctx)
}

func (r *uncheckedRuntime) ObserveForRecovery(ctx context.Context) (gpuruntime.Snapshot, error) {
	return r.Observe(ctx)
}

// fakeRuntime cannot check limits or run trainers. Fakes that can
// override these methods.
func (r *fakeRuntime) VerifyLaunchLimits(context.Context, control.Workload) error {
	return gpuruntime.ErrLimitUnchecked
}

func (r *fakeRuntime) VerifyLimitProtection(context.Context, control.Workload) error {
	return gpuruntime.ErrLimitUnchecked
}

func (r *fakeRuntime) LimitOutcome(context.Context, control.Workload) gpuruntime.LimitOutcome {
	return gpuruntime.LimitOutcome{Class: gpuruntime.LimitOutcomeNone, Detail: "the fake cannot read it"}
}

func (r *fakeRuntime) LimitStopReleased(context.Context, control.Workload) error {
	return gpuruntime.ErrLimitUnchecked
}

var errFakeTraining = errors.New("the fake runs no trainer")

func (r *fakeRuntime) StartTraining(context.Context, control.WorkloadProfile, control.TrainingJob) (string, error) {
	return "", errFakeTraining
}

func (r *fakeRuntime) TrainingTerminal(context.Context, control.WorkloadProfile, control.TrainingJob) (control.TrainingTerminal, error) {
	return control.TrainingTerminal{}, errFakeTraining
}

func (r *fakeRuntime) ReleaseTraining(context.Context, control.WorkloadProfile) error {
	return errFakeTraining
}

func (r *fakeRuntime) TrainingMemoryPeak(context.Context, control.WorkloadProfile, control.TrainingJob) (uint64, error) {
	return 0, errFakeTraining
}
