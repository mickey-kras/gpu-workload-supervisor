package supervisor

import (
	"fmt"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

func verifySnapshot(target control.Workload, snapshot gpuruntime.Snapshot) error {
	if snapshot.Workloads != nil {
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
	if snapshot.MediaExclusive && snapshot.MediaReady && target != control.WorkloadMedia {
		return fmt.Errorf("%w: media runtime remains active", ErrStateVerification)
	}
	switch target {
	case control.WorkloadText:
		if !snapshot.TextActive {
			return fmt.Errorf("%w: text runtime is not active", ErrStateVerification)
		}
	case control.WorkloadMedia:
		if snapshot.TextActive || !snapshot.MediaReady {
			return fmt.Errorf("%w: media runtime is not exclusively ready", ErrStateVerification)
		}
	case control.WorkloadIdle:
		if snapshot.TextActive {
			return fmt.Errorf("%w: text runtime remains active", ErrStateVerification)
		}
	}
	return nil
}

func observedWorkload(state control.State, snapshot gpuruntime.Snapshot) (control.Workload, error) {
	if snapshot.Workloads != nil {
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
	if snapshot.MediaExclusive && snapshot.MediaReady {
		if snapshot.TextActive {
			return control.WorkloadUnknown, ErrInvariant
		}
		return control.WorkloadMedia, nil
	}
	if snapshot.TextActive {
		if state.ActiveWorkload == control.WorkloadMedia {
			return control.WorkloadUnknown, ErrInvariant
		}
		return control.WorkloadText, nil
	}
	if state.ActiveWorkload == control.WorkloadMedia && snapshot.MediaReady {
		return control.WorkloadMedia, nil
	}
	return control.WorkloadIdle, nil
}
