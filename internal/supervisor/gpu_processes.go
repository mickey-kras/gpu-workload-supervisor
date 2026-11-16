package supervisor

import (
	"context"
	"errors"
	"fmt"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

// gpuProcessRuntime is the GPU process check before each start. The runtime
// owns the one check; the controller runs it before any effect, and the
// runtime runs it again just before the start.
type gpuProcessRuntime interface {
	// CheckGPUFree refuses a start while a process outside the supervised
	// cgroups uses the target GPU, or while the check cannot complete.
	CheckGPUFree(context.Context, control.Workload) error
	// CloseGPUProcess sends SIGTERM to one process of the current user on
	// the target GPU, then checks the GPU again.
	CloseGPUProcess(context.Context, control.Workload, int) error
}

// ErrGPURefused marks a start that the GPU process check refused before any
// effect: the current workload keeps running and nothing was written. A
// check that fails later, in the runtime start, keeps the usual transition
// failure instead.
var ErrGPURefused = errors.New("start refused before any change")

// refuseGPUInUse runs the GPU process check of target before any effect.
// Its error keeps the runtime type, so the operator can show the processes
// or the failure. Nothing changed, and the current workload keeps running.
func (c *Controller) refuseGPUInUse(ctx context.Context, target control.Workload) error {
	if target == control.WorkloadIdle || target == control.WorkloadUnknown {
		return nil
	}
	err := c.runAction(ctx, func(actionCtx context.Context) error {
		return c.runtime.CheckGPUFree(actionCtx, target)
	})
	if err != nil {
		return fmt.Errorf("%w: %w", ErrGPURefused, err)
	}
	return nil
}

// CloseGPUProcess is the operator "Close it" action for one process that
// blocks the start of target. The runtime only signals a process of the
// current user, and only with SIGTERM. The result is the check again:
// nil when the GPU is free.
func (c *Controller) CloseGPUProcess(ctx context.Context, target control.Workload, pid int) error {
	if target == control.WorkloadIdle || target == control.WorkloadUnknown || !c.configuredTarget(target) {
		return fmt.Errorf("%w %q", ErrInvalidTarget, target)
	}
	return c.runAction(ctx, func(actionCtx context.Context) error {
		return c.runtime.CloseGPUProcess(actionCtx, target, pid)
	})
}
