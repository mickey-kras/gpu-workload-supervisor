package runtime

import (
	"context"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

type Snapshot struct {
	TextActive bool
	MediaReady bool
}

type Manager interface {
	Observe(context.Context) (Snapshot, error)
	Start(context.Context, control.Workload) error
	Stop(context.Context, control.Workload) error
	// StopForRecovery shuts down both runtime units, including the media UI.
	// Normal Stop(media) only releases models and leaves that unit running.
	StopForRecovery(context.Context) error
	Healthy(context.Context, control.Workload) error
	Released(context.Context) error
}
