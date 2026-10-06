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
	Workloads      map[control.Workload]WorkloadObservation
	TextActive     bool
	MediaReady     bool
	MediaExclusive bool
}

type Manager interface {
	Observe(context.Context) (Snapshot, error)
	Start(context.Context, control.Workload) error
	Stop(context.Context, control.Workload) error
	// StopForRecovery shuts down both runtime units, including the media UI.
	// Normal Stop(media) follows the configured media stop policy.
	StopForRecovery(context.Context) error
	Healthy(context.Context, control.Workload) error
	ReleasedFor(context.Context, control.Workload) error
	Preflight(context.Context) error
}

func (s Snapshot) AnyActive() bool {
	if s.TextActive || s.MediaReady {
		return true
	}
	for _, o := range s.Workloads {
		if o.Active {
			return true
		}
	}
	return false
}
