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
