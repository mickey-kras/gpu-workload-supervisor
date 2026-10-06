package supervisor

import (
	"context"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

func (c *Controller) rollback(ctx context.Context, transitionID string, previous control.State) error {
	if c.config.Catalog != nil {
		return c.rollbackCatalog(ctx, transitionID, previous)
	}
	snapshot, err := c.observe(ctx)
	if err != nil {
		return err
	}
	switch previous.ActiveWorkload {
	case control.WorkloadText:
		return c.rollbackText(ctx, transitionID, snapshot)
	case control.WorkloadMedia:
		return c.rollbackMedia(ctx, transitionID, snapshot)
	default:
		return c.rollbackIdle(ctx, transitionID, snapshot)
	}
}

func (c *Controller) rollbackText(ctx context.Context, transitionID string, snapshot gpuruntime.Snapshot) error {
	if !snapshot.TextActive {
		if err := c.waitExclusiveRelease(ctx, snapshot); err != nil {
			return err
		}
		if err := c.effect(ctx, transitionID, control.PhaseReconciling, "rollback start text", func(actionCtx context.Context) error {
			return c.runtime.Start(actionCtx, control.WorkloadText)
		}); err != nil {
			return err
		}
	}
	return c.waitReady(ctx, control.WorkloadText, c.now().Add(c.config.CleanupTimeout))
}

func (c *Controller) rollbackMedia(ctx context.Context, transitionID string, snapshot gpuruntime.Snapshot) error {
	if snapshot.TextActive {
		if err := c.stopTextEffect(ctx, transitionID); err != nil {
			return err
		}
	}
	if !snapshot.MediaReady {
		if err := c.waitExclusiveRelease(ctx, snapshot); err != nil {
			return err
		}
		if err := c.effect(ctx, transitionID, control.PhaseReconciling, "rollback start media", func(actionCtx context.Context) error {
			return c.runtime.Start(actionCtx, control.WorkloadMedia)
		}); err != nil {
			return err
		}
	}
	return c.waitReady(ctx, control.WorkloadMedia, c.now().Add(c.config.CleanupTimeout))
}

func (c *Controller) rollbackIdle(ctx context.Context, transitionID string, snapshot gpuruntime.Snapshot) error {
	if snapshot.TextActive {
		if err := c.stopTextEffect(ctx, transitionID); err != nil {
			return err
		}
	}
	// A failed media start may have allocated memory without reaching readiness.
	if err := c.effect(ctx, transitionID, control.PhaseReconciling, "rollback stop media", func(actionCtx context.Context) error {
		return c.runtime.Stop(actionCtx, control.WorkloadMedia)
	}); err != nil {
		return err
	}
	return c.waitReleased(ctx, c.now().Add(c.config.CleanupTimeout))
}

func (c *Controller) stopTextEffect(ctx context.Context, transitionID string) error {
	return c.effect(ctx, transitionID, control.PhaseReconciling, "rollback stop text", func(actionCtx context.Context) error {
		return c.runtime.Stop(actionCtx, control.WorkloadText)
	})
}

func (c *Controller) waitExclusiveRelease(ctx context.Context, snapshot gpuruntime.Snapshot) error {
	if !snapshot.MediaExclusive {
		return nil
	}
	return c.waitReleased(ctx, c.now().Add(c.config.CleanupTimeout))
}
