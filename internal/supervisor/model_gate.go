package supervisor

import (
	"context"
	"errors"
	"fmt"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

// refuseModelCheck is the model check gate of a new launch. Switch,
// ownership changes and activation run it before any effect, so a refused
// launch changes nothing. A verify-only recovery starts nothing, so it skips
// the gate. A workload that is already running keeps running.
//
// The stored result of a vLLM workload is keyed by the workload ID. A
// stored result must allow the launch: the file, identity and template
// checks passed, and the binding pins the template that the check passed,
// see modelcheck.Record.AllowLaunch. A result that cannot be read refuses
// the launch. A workload without a stored result still launches: setup
// writes a result only when the operator runs the checks (gpu-setup
// model-check), so a gate that needs a result waits for a later #583
// change.
func (c *Controller) refuseModelCheck(ctx context.Context, target control.Workload, verifyOnly bool) error {
	p, ok := c.config.Catalog.Catalog.Profile(target)
	if verifyOnly || !ok || p.NativeModel == nil {
		return nil
	}
	record, err := c.store.ModelCheck(ctx, string(p.ID))
	if errors.Is(err, store.ErrNoModelCheck) {
		return nil
	}
	if err != nil {
		return gpuruntime.UnitError(p, fmt.Errorf("read the model check result: %w", err))
	}
	root := ""
	if p.NativeModel.Owned != nil {
		root = p.NativeModel.Owned.ModelPath
	}
	if err := record.AllowLaunch(root, p.NativeModel.ChatTemplateSHA256); err != nil {
		return gpuruntime.UnitError(p, err)
	}
	return nil
}
