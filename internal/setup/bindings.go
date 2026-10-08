package setup

import (
	"context"
	"errors"
	"os"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

// VerifyBindings reads service, launch-file and cgroup evidence without starting
// workloads. Existing adopted bindings are checked during owned-launch previews;
// the complete catalog is preflighted again during Apply. Model readiness is checked separately before runtime admission.
func VerifyBindings(ctx context.Context, request Request) error {
	return SystemBackend().VerifyBindings(ctx, request)
}

func (b Backend) VerifyBindings(ctx context.Context, request Request) error {
	home, err := Home()
	if err != nil {
		return err
	}
	return b.verifyBindings(ctx, home, request)
}

func (b Backend) verifyBindings(ctx context.Context, home string, request Request) error {
	if err := Validate(request); err != nil {
		return err
	}
	var accepted control.CatalogSnapshot
	if _, err := os.Stat(request.Profile.StatePath); !errors.Is(err, os.ErrNotExist) {
		var err error
		accepted, err = ReadCatalog(ctx, request.Profile.StatePath)
		if err != nil {
			return err
		}
	}
	plan, err := b.planOwnedUnits(request, accepted, home)
	if err != nil {
		return err
	}
	removals := map[string]string{}
	for _, name := range plan.Deletes {
		removals[name] = plan.proven[name]
	}
	manager, err := b.makeRuntime(request)
	if err != nil {
		return err
	}
	for _, p := range request.Catalog.Profiles {
		if p.NativeModel == nil || p.NativeModel.Owned == nil {
			continue
		}
		preview, ok := manager.(interface {
			PreflightAdopted(context.Context, map[string]string) error
		})
		if !ok {
			return errors.New("runtime cannot verify adopted bindings alongside pending owned launches")
		}
		return preview.PreflightAdopted(ctx, removals)
	}
	if preview, ok := manager.(interface {
		PreflightWithOwnedRemovals(context.Context, map[string]string) error
	}); ok && len(removals) > 0 {
		return preview.PreflightWithOwnedRemovals(ctx, removals)
	}
	return manager.Preflight(ctx)
}
