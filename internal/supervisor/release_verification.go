package supervisor

import (
	"context"
	"errors"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

func (c *Controller) waitReleasedFor(ctx context.Context, target control.Workload, deadline time.Time) error {
	verifyCtx, cancel := context.WithTimeout(ctx, deadline.Sub(c.now()))
	defer cancel()
	ticker := time.NewTicker(c.config.PollInterval)
	defer ticker.Stop()
	var lastErr error
	for {
		lastErr = c.releasedFor(verifyCtx, target)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if verifyCtx.Err() != nil || !c.now().Before(deadline) {
			return errors.Join(ErrVerifyTimeout, lastErr)
		}
		if lastErr == nil || errors.Is(lastErr, gpuruntime.ErrUnloadUnverified) {
			return lastErr
		}
		select {
		case <-verifyCtx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.Join(ErrVerifyTimeout, lastErr)
		case <-ticker.C:
		}
	}
}
