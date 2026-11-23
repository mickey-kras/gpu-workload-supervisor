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

// refuseModelCheck is the model check gate of a new launch. Switch,
// ownership changes and activation run it before any effect, so a refused
// launch changes nothing. A transition that starts nothing skips the gate:
// a verify-only recovery, an ownership transfer that preserves the running
// workload, and a switch to the workload that already runs. The start step
// checks again before it starts a target.
//
// The stored result of a vLLM workload is keyed by the workload ID. A
// stored result must allow the launch: the file, identity and template
// checks passed, and the binding pins the template that the check passed,
// see modelcheck.Record.AllowLaunch. A result that cannot be read refuses
// the launch, and so does a workload without a stored result: the operator
// runs the checks in setup (gpu-setup model-check) before the first launch.
func (c *Controller) refuseModelCheck(ctx context.Context, target control.Workload, startsNothing bool) error {
	p, ok := c.config.Catalog.Catalog.Profile(target)
	if startsNothing || !ok || p.NativeModel == nil {
		return nil
	}
	record, err := c.store.ModelCheck(ctx, string(p.ID))
	if errors.Is(err, store.ErrNoModelCheck) && p.NativeModel.Owned == nil {
		// Setup has no model directory to check for an adopted launch.
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
