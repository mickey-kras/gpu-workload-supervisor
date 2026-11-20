package supervisor

import (
	"context"
)

func (c *Controller) preflight(ctx context.Context) error {
	probe, cancel := context.WithTimeout(ctx, c.config.ActionTimeout)
	defer cancel()
	return c.runtime.Preflight(probe)
}

// preflightForStop checks unit identity and placement without authorizing
// execution of an older managed service. Other runtimes use normal preflight.
func (c *Controller) preflightForStop(ctx context.Context) error {
	if runtime, ok := c.runtime.(interface{ PreflightForStop(context.Context) error }); ok {
		probe, cancel := context.WithTimeout(ctx, c.config.ActionTimeout)
		defer cancel()
		return runtime.PreflightForStop(probe)
	}
	return c.preflight(ctx)
}
