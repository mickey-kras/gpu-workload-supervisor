package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

// ErrLimitsNeedConversion refuses limits on a unit that the accepted
// catalog adopts. Setup gives limits only to units that it owns. The
// conversion to an owned unit and the limits are two separate applies.
var ErrLimitsNeedConversion = errors.New("resource limits need a supervisor-owned unit")

// errHostCapWithoutLimits refuses host cap values in a request with no
// unit with limits: no root step is needed then.
var errHostCapWithoutLimits = errors.New("hostCap needs a unit with resource limits; remove hostCap")

// limitCheckTimeout bounds the host checks of preview.
const limitCheckTimeout = 30 * time.Second

// limitHostChecker runs the setup checks of the runtime for units with
// limits.
type limitHostChecker interface {
	CheckLimitHost(context.Context, control.Workload) error
	HostCapStep(context.Context, *gpuruntime.HostCapValues) (gpuruntime.HostCapStep, error)
}

// limitedProfiles gives the profiles with limits.
func limitedProfiles(catalog control.Catalog) []control.WorkloadProfile {
	return catalog.LimitedProfiles()
}

// requestReserve is the reserve of the request profile, or the default.
func requestReserve(request Request) gpuruntime.LimitReserve {
	return gpuruntime.EffectiveReserve(request.Profile.LimitReserve)
}

// validateLimits checks the host cap values of the request against its
// units with limits and the reserve. The host checks of the runtime run
// later, in preview and apply.
func validateLimits(request Request) error {
	if request.HostCap == nil {
		return nil
	}
	limited := limitedProfiles(request.Catalog)
	if len(limited) == 0 {
		return errHostCapWithoutLimits
	}
	var limits []control.ResourceLimits
	for _, p := range limited {
		limits = append(limits, p.NativeModel.Owned.Limits)
	}
	_, _, err := requestReserve(request).HostCapFor(*request.HostCap, limits)
	return err
}

// refuseLimitConversion refuses limits on a unit that the accepted catalog
// adopts, also in the apply that converts it. A conversion gives the unit
// a new name, so the workload ID matches too.
func refuseLimitConversion(accepted, requested control.Catalog) error {
	for _, p := range limitedProfiles(requested) {
		for _, old := range accepted.Profiles {
			if (old.Unit == p.Unit || old.ID == p.ID) && (old.NativeModel == nil || old.NativeModel.Owned == nil) {
				return fmt.Errorf("%w: %s (unit %s) is adopted; convert it to a managed unit through setup preview and apply first, then add the limits in a second apply",
					ErrLimitsNeedConversion, p.ID, old.Unit)
			}
		}
	}
	return nil
}

// acceptedCatalog reads the accepted catalog for preview. A missing state
// file has none.
func acceptedCatalog(ctx context.Context, statePath string) (control.Catalog, error) {
	if _, err := os.Lstat(statePath); errors.Is(err, os.ErrNotExist) {
		return control.Catalog{}, nil
	}
	snapshot, err := ReadCatalog(ctx, statePath)
	return snapshot.Catalog, err
}

// limitChecker gives the setup checks of the runtime. A runtime without
// them cannot accept limits.
func limitChecker(manager gpuruntime.Manager) (limitHostChecker, error) {
	checker, ok := manager.(limitHostChecker)
	if !ok {
		return nil, gpuruntime.ErrLimitUnchecked
	}
	return checker, nil
}

// previewHostCap gives the host cap step of a preview. It refuses limits
// on an adopted unit. A request without limits needs no root step.
func previewHostCap(manager gpuruntime.Manager, request Request) (*gpuruntime.HostCapStep, error) {
	if len(limitedProfiles(request.Catalog)) == 0 {
		return notNeeded(request), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), limitCheckTimeout)
	defer cancel()
	accepted, err := acceptedCatalog(ctx, request.Profile.StatePath)
	if err != nil {
		return nil, err
	}
	if err := refuseLimitConversion(accepted, request.Catalog); err != nil {
		return nil, err
	}
	checker, err := limitChecker(manager)
	if err != nil {
		return nil, err
	}
	step, err := checker.HostCapStep(ctx, request.HostCap)
	return &step, err
}

// acceptLimits runs before any unit write of apply. It refuses limits on an
// adopted unit and runs the setup checks of the runtime for each unit with
// limits: the controllers, the host cap, the reserve and the I/O device.
func (b Backend) acceptLimits(ctx context.Context, request Request, accepted control.Catalog) error {
	limited := limitedProfiles(request.Catalog)
	if len(limited) == 0 {
		return nil
	}
	if err := refuseLimitConversion(accepted, request.Catalog); err != nil {
		return err
	}
	manager, err := b.makeRuntime(request)
	if err != nil {
		return err
	}
	checker, err := limitChecker(manager)
	if err != nil {
		return err
	}
	for _, p := range limited {
		if err := checker.CheckLimitHost(ctx, p.ID); err != nil {
			return gpuruntime.UnitError(p, err)
		}
	}
	return nil
}

// sameProfile reads operator.json again under the state lock and refuses
// a profile that changed since the first read. The reserve of the runtime
// then belongs to the same committed setup as the catalog.
func sameProfile(home string, before []byte) error {
	data, err := privateRead(filepath.Join(home, ".config/gpu-workload-supervisor/operator.json"))
	if err != nil {
		return err
	}
	if !bytes.Equal(data, before) {
		return errProfileChanged
	}
	return nil
}

// errProfileChanged refuses a profile that setup changed during the read.
var errProfileChanged = errors.New("the operator profile changed during the check; retry")

// notNeeded is the host cap step of a request without limits.
func notNeeded(request Request) *gpuruntime.HostCapStep {
	return &gpuruntime.HostCapStep{Status: gpuruntime.HostCapNotNeeded, Reserve: requestReserve(request)}
}

// discoverHostCap adds the host cap step of the catalog of the discovered
// request: the accepted catalog, or the request of a pending activation. A
// catalog without limits needs no root step.
func (b Backend) discoverHostCap(ctx context.Context, result *Discovery) {
	if len(limitedProfiles(result.Request.Catalog)) == 0 {
		result.HostCap = notNeeded(result.Request)
		return
	}
	step, err := b.hostCapStep(ctx, result.Request)
	if err != nil {
		result.Errors = append(result.Errors, "The host protection of this account cannot be checked: "+err.Error())
		return
	}
	result.HostCap = &step
}

// hostCapStep runs the host cap step of the runtime for request, without
// requested values.
func (b Backend) hostCapStep(ctx context.Context, request Request) (gpuruntime.HostCapStep, error) {
	manager, err := b.makeRuntime(request)
	if err != nil {
		return gpuruntime.HostCapStep{}, err
	}
	checker, err := limitChecker(manager)
	if err != nil {
		return gpuruntime.HostCapStep{}, err
	}
	return checker.HostCapStep(ctx, nil)
}
