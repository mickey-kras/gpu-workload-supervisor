package runtime

import (
	"context"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

type Snapshot struct {
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
	Released(context.Context) error
}

// TargetReleaseVerifier permits a destination runtime to remain alive when its
// policy supports that. Other runtimes retain the all-workloads Released check.
type TargetReleaseVerifier interface {
	ReleasedFor(context.Context, control.Workload) error
}
