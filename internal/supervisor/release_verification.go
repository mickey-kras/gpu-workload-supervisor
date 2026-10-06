package supervisor

import (
	"context"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func (c *Controller) waitReleasedFor(ctx context.Context, target control.Workload, deadline time.Time) error {
	return c.pollUntil(ctx, deadline, func(probeCtx context.Context) error {
		return c.releasedFor(probeCtx, target)
	})
}
