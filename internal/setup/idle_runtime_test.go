package setup

import (
	"context"
	"errors"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

func (r idleRuntime) ObserveForRecovery(ctx context.Context) (gpuruntime.Snapshot, error) {
	return r.Observe(ctx)
}

// idleRuntime cannot check limits or run trainers.
func (r idleRuntime) VerifyLaunchLimits(context.Context, control.Workload) error {
	return gpuruntime.ErrLimitUnchecked
}

func (r idleRuntime) VerifyLimitProtection(context.Context, control.Workload) error {
	return gpuruntime.ErrLimitUnchecked
}

func (r idleRuntime) LimitOutcome(context.Context, control.Workload) gpuruntime.LimitOutcome {
	return gpuruntime.LimitOutcome{Class: gpuruntime.LimitOutcomeNone}
}

func (r idleRuntime) LimitStopReleased(context.Context, control.Workload) error {
	return gpuruntime.ErrLimitUnchecked
}

var errFakeTraining = errors.New("the fake runs no trainer")

func (r idleRuntime) StartTraining(context.Context, control.WorkloadProfile, control.TrainingJob) (string, error) {
	return "", errFakeTraining
}

func (r idleRuntime) TrainingTerminal(context.Context, control.WorkloadProfile, control.TrainingJob) (control.TrainingTerminal, error) {
	return control.TrainingTerminal{}, errFakeTraining
}

func (r idleRuntime) ReleaseTraining(context.Context, control.WorkloadProfile) error {
	return errFakeTraining
}

func (r idleRuntime) TrainingMemoryPeak(context.Context, control.WorkloadProfile, control.TrainingJob) (uint64, error) {
	return 0, errFakeTraining
}
