package runtime

import (
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

type WorkloadObservation struct {
	Active    bool
	Sleeping  bool
	Exclusive bool
}

// Running reports whether the unit still owns a live workload process.
func (o WorkloadObservation) Running() bool { return o.Active || o.Sleeping }

type Snapshot struct {
	Workloads map[control.Workload]WorkloadObservation
}

func (s Snapshot) AnyActive() bool {
	for _, o := range s.Workloads {
		if o.Active {
			return true
		}
	}
	return false
}
