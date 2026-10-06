package supervisor

import (
	"context"
)

func (c *Controller) preflight(ctx context.Context) error {
	probe, cancel := context.WithTimeout(ctx, c.config.ActionTimeout)
	defer cancel()
	return c.runtime.Preflight(probe)
}
