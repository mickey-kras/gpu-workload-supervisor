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

// reloadProfile reads the operator profile again under the state lock.
// Tests replace it.
var reloadProfile = LoadProfile

// newNativeManager builds the runtime of a session. Tests replace it.
var newNativeManager = gpuruntime.NewSystemdManager

// openNativeSession runs under the state lock of Handle. The caller read p
// before that lock, and the gateway reads it once at its start, so a setup
// apply can commit a new catalog and a new limit reserve in between. The
// session reads the profile again under the lock and uses its reserve, so
// the reserve and the catalog come from the same committed setup. A
// profile that it cannot read, or one for another state path, refuses the
// session.
func openNativeSession(ctx context.Context, p Profile) (Session, error) {
	if p.Validate() != nil || deployment.Check(p.StatePath, p.ActivatedRelease) != nil {
		return Session{}, ErrIncompatibleConfiguration
	}
	current, err := reloadProfile()
	if err != nil || current.StatePath != p.StatePath {
		return Session{}, ErrIncompatibleConfiguration
	}
	p.LimitReserve = current.LimitReserve
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
	manager, err := newNativeManager(p.SystemdConfig(&snap.Catalog))
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
	return Session{Backend: controller, Revision: snap.Revision, Workloads: ws, Close: s.Close, PolicyStore: s, IdlePolicyConfigurable: false}, nil
}
