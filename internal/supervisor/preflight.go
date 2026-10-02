package supervisor

import (
	"context"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

func (c *Controller) preflight(ctx context.Context) error {
	if verifier, ok := c.runtime.(gpuruntime.CapabilityPreflight); ok {
		probe, cancel := context.WithTimeout(ctx, c.config.ActionTimeout)
		defer cancel()
		return verifier.Preflight(probe)
	}
	return nil
}
