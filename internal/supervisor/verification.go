package supervisor

import (
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

func verifySnapshot(target control.Workload, snapshot gpuruntime.Snapshot) error {
	for id, o := range snapshot.Workloads {
		if id != target && o.Active && o.Exclusive {
			return ErrStateVerification
		}
	}
	if target != control.WorkloadIdle && !snapshot.Workloads[target].Active {
		return ErrStateVerification
	}
	return nil
}

func observedWorkload(state control.State, snapshot gpuruntime.Snapshot) (control.Workload, error) {
	active := control.WorkloadIdle
	for id, o := range snapshot.Workloads {
		if o.Active && (o.Exclusive || id == state.ActiveWorkload) {
			if active != control.WorkloadIdle {
				return control.WorkloadUnknown, ErrInvariant
			}
			active = id
		}
	}
	return active, nil
}
