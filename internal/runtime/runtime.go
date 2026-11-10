package runtime

import (
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

type WorkloadObservation struct {
	Active    bool
	Exclusive bool
}

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
