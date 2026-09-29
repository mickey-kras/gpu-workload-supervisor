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
	Healthy(context.Context, control.Workload) error
}
