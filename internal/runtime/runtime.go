package runtime

import (
	"context"
	"errors"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

var ErrAmbiguousState = errors.New("text and media workloads are active")

type Snapshot struct {
	TextActive  bool
	MediaActive bool
}

func (s Snapshot) Workload() (control.Workload, error) {
	switch {
	case s.TextActive && s.MediaActive:
		return control.WorkloadUnknown, ErrAmbiguousState
	case s.TextActive:
		return control.WorkloadText, nil
	case s.MediaActive:
		return control.WorkloadMedia, nil
	default:
		return control.WorkloadIdle, nil
	}
}

type Manager interface {
	Observe(context.Context) (Snapshot, error)
	Start(context.Context, control.Workload) error
	Stop(context.Context, control.Workload) error
	Healthy(context.Context, control.Workload) error
}
