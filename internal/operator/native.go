package operator

import (
	"context"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/supervisor"
	"time"
)

var ErrIncompatibleConfiguration = errors.New("incompatible operator configuration")

// NativeService reads only the durable accepted catalog. No request or edited
// setup file can select a runtime catalog or bypass activation.
func NativeService(p Profile) Service {
	return Service{StatePath: p.StatePath, StatusTimeout: time.Duration(p.StatusTimeoutSeconds) * time.Second, OperationTimeout: time.Duration(p.OperationTimeoutSeconds) * time.Second, Open: func(ctx context.Context) (Session, error) {
		return openNativeSession(ctx, p)
	}}
}

func openNativeSession(ctx context.Context, p Profile) (Session, error) {
	if ValidateProfile(p) != nil || deployment.Check(p.StatePath, p.ActivatedRelease) != nil {
		return Session{}, ErrIncompatibleConfiguration
	}
	s, err := store.Open(ctx, p.StatePath)
	if err != nil {
		return Session{}, err
	}
	fail := func(err error) (Session, error) { s.Close(); return Session{}, err }
	snap, err := s.Catalog(ctx)
	if err != nil {
		return fail(err)
	}
	if snap.Revision == "" || snap.Catalog.Validate() != nil {
		return fail(ErrIncompatibleConfiguration)
	}
	manager, err := gpuruntime.NewSystemdManager(gpuruntime.SystemdConfig{Catalog: &snap.Catalog, SystemctlPath: p.SystemctlPath, NvidiaSMIPath: p.NvidiaSMIPath, GPUIndex: p.GPUIndex, CapacityHeadroomMiB: p.CapacityHeadroomMiB, HealthTimeout: 10 * time.Second})
	if err != nil {
		return fail(ErrIncompatibleConfiguration)
	}
	controller, err := supervisor.New(s, manager, supervisor.Config{Catalog: &snap, DrainTimeout: 5 * time.Minute, VerifyTimeout: 5 * time.Minute, ActionTimeout: 2 * time.Minute, CleanupTimeout: 2 * time.Minute, FinalizeTimeout: 10 * time.Second, PollInterval: 250 * time.Millisecond})
	if err != nil {
		return fail(err)
	}
	ws := []Workload{{ID: "idle", Label: "Idle"}}
	for _, p := range snap.Catalog.Profiles {
		ws = append(ws, Workload{p.ID, p.Label})
	}
	return Session{Backend: controller, Revision: snap.Revision, Workloads: ws, Close: s.Close}, nil
}
