package supervisor

import (
	"context"
	"errors"
	"fmt"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/modelcheck"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

// refuseModelCheck is the model check gate for a new launch. Switch,
// ownership changes and activation run it before any side effect, so a
// refused launch has no effect. Transitions that start nothing skip it: a
// verify-only recovery, an ownership transfer that keeps the running
// workload, and a switch to the workload that already runs. The start step
// checks again before it starts a target.
//
// A vLLM workload's stored result is keyed by workload ID and must allow the
// launch (see modelcheck.Record.AllowLaunch). An unreadable result blocks the
// launch, as does a missing one: the operator runs gpu-setup model-check
// before the first launch.
func (c *Controller) refuseModelCheck(ctx context.Context, target control.Workload, startsNothing bool) error {
	p, ok := c.config.Catalog.Catalog.Profile(target)
	if startsNothing || !ok || p.NativeModel == nil {
		return nil
	}
	record, err := c.store.ModelCheck(ctx, string(p.ID))
	if errors.Is(err, store.ErrNoModelCheck) && p.NativeModel.Owned == nil {
		return gpuruntime.UnitError(p, fmt.Errorf("%w: setup cannot check the model files of an adopted vLLM service; set it up as a supervisor-managed launch", modelcheck.ErrLaunchRefused))
	}
	if errors.Is(err, store.ErrNoModelCheck) {
		return gpuruntime.UnitError(p, fmt.Errorf("%w: no model check result; run the model checks in setup", modelcheck.ErrLaunchRefused))
	}
	if err != nil {
		return gpuruntime.UnitError(p, fmt.Errorf("read the model check result: %w", err))
	}
	root := ""
	if p.NativeModel.Owned != nil {
		root = p.NativeModel.Owned.ModelPath
	}
	if err := record.AllowLaunchRecord(root, p.NativeModel.ChatTemplateSHA256); err != nil {
		return gpuruntime.UnitError(p, err)
	}
	if err := c.modelFilesUnchanged(record); err != nil {
		return gpuruntime.UnitError(p, err)
	}
	return nil
}
