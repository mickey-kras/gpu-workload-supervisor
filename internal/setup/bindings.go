package setup

import (
	"context"
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
	return manager.Preflight(ctx)
}
