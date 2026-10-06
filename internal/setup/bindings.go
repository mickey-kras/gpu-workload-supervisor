package setup

import (
	"context"
	"errors"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

// VerifyBindings reads service, launch-file and cgroup evidence without starting
// workloads. Model readiness is checked separately before runtime admission.
func VerifyBindings(ctx context.Context, request Request) error {
	return SystemBackend().VerifyBindings(ctx, request)
}

func (b Backend) VerifyBindings(ctx context.Context, request Request) error {
	if err := Validate(request); err != nil {
		return err
	}
	manager, err := b.makeRuntime(request)
	if err != nil {
		return err
	}
	verifier, ok := manager.(gpuruntime.CapabilityPreflight)
	if !ok {
		return errors.New("runtime cannot verify launch bindings")
	}
	return verifier.Preflight(ctx)
}
