package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

// The limit errors of Start name the workload and the unit, and a slice
// property mismatch names the slice unit.
func TestLimitErrorsNameTheUnit(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.edit(f.sliceShow(), "TasksMax=16384", "TasksMax=9")
	prefix := string(f.p.ID) + " (" + f.p.Unit + "): "
	err := f.m.VerifyLaunchLimits(context.Background(), f.p.ID)
	if !errors.Is(err, ErrHostCap) || !strings.Contains(err.Error(), `TasksMax of user-1000.slice is "9"`) {
		t.Fatalf("slice property mismatch = %v", err)
	}
	if err := f.start(t); !errors.Is(err, ErrHostCap) || !strings.HasPrefix(err.Error(), prefix) || f.started() {
		t.Fatalf("start with a slice property mismatch = %v", err)
	}
}

// A workload that the runtime catalog does not have fails the checks
// closed: the supervisor asks only for a unit that its catalog limits.
func TestLimitChecksOfUnknownWorkloadFail(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	ctx := context.Background()
	for name, check := range map[string]func(context.Context, control.Workload) error{"launch": f.m.VerifyLaunchLimits, "protection": f.m.VerifyLimitProtection} {
		if err := check(ctx, "unknown"); !errors.Is(err, ErrLimitUnchecked) || !DefiniteLimitFailure(err) || !strings.Contains(err.Error(), "unknown") {
			t.Errorf("%s checks of an unknown workload = %v", name, err)
		}
	}
	if len(f.r.calls) != 0 {
		t.Fatalf("checks of an unknown workload ran commands: %v", f.r.calls)
	}
}

// The schema accepts limits without memoryMaxMiB, tasksMax or
// cpuQuotaPercent, so setup can read and remove them. Both launch entry
// points refuse such a unit as a definite failure. The error names each
// missing control by its catalog field, with "it" for one and "them" for a
// list.
func TestLaunchRequiresCappedControls(t *testing.T) {
	for name, c := range map[string]struct {
		limits control.ResourceLimits
		want   string
	}{
		"no memoryMaxMiB": {control.ResourceLimits{Version: 1, TasksMax: 4096, CPUQuotaPercent: 100}, "needs memoryMaxMiB to start; set it"},
		"no tasksMax":     {control.ResourceLimits{Version: 1, MemoryMaxMiB: 8192, CPUQuotaPercent: 100}, "needs tasksMax to start; set it"},
		"no cpuQuotaPercent": {control.ResourceLimits{Version: 1, MemoryMaxMiB: 8192, TasksMax: 4096},
			"needs cpuQuotaPercent to start; set it"},
		"only tasksMax": {control.ResourceLimits{Version: 1, TasksMax: 4096}, "needs memoryMaxMiB and cpuQuotaPercent to start; set them"},
		"only cpuQuotaPercent": {control.ResourceLimits{Version: 1, CPUQuotaPercent: 100},
			"needs memoryMaxMiB and tasksMax to start; set them"},
	} {
		if err := c.limits.Validate(); err != nil {
			t.Fatalf("%s: schema rejects the limits: %v", name, err)
		}
		f := newLimitFixture(t, c.limits, "")
		f.cgroup(t, limitSlice, "io.max", "")
		for entry, check := range map[string]func() error{
			"before unload": func() error { return f.m.VerifyLaunchLimits(context.Background(), f.p.ID) },
			"start":         func() error { return f.m.Start(context.Background(), f.p.ID) },
		} {
			if err := check(); !errors.Is(err, ErrLimitReserve) || !DefiniteLimitFailure(err) || !strings.Contains(err.Error(), c.want) || f.started() {
				t.Errorf("%s, %s = %v", name, entry, err)
			}
		}
	}
	if err := requireCappedControls(control.ResourceLimits{}); !strings.Contains(err.Error(), "needs memoryMaxMiB, tasksMax and cpuQuotaPercent to start; set them") {
		t.Fatalf("no capped controls = %v", err)
	}
}
