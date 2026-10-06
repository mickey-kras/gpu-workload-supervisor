package runtime

import (
	"context"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

type WorkloadObservation struct {
	Active    bool
	Exclusive bool
}

type Snapshot struct {
	Workloads map[control.Workload]WorkloadObservation
}

type Manager interface {
	Observe(context.Context) (Snapshot, error)
	Start(context.Context, control.Workload) error
	Stop(context.Context, control.Workload) error
	// StopForRecovery shuts down every configured runtime unit, including the
	// media UI. Normal Stop follows the profile's adapter policy.
	StopForRecovery(context.Context) error
	Healthy(context.Context, control.Workload) error
	ReleasedFor(context.Context, control.Workload) error
	Preflight(context.Context) error
}

func (s Snapshot) AnyActive() bool {
	for _, o := range s.Workloads {
		if o.Active {
			return true
		}
	}
	return false
}
