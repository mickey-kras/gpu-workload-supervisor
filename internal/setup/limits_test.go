package setup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

// limitRuntime is an idle runtime with the setup checks of units with
// limits. It records the checked units and the requested host cap values.
type limitRuntime struct {
	idleRuntime
	checkErr error
	step     gpuruntime.HostCapStep
	stepErr  error
	checked  *[]control.Workload
	wanted   **gpuruntime.HostCapValues
}

func (r limitRuntime) CheckLimitHost(_ context.Context, workload control.Workload) error {
	if r.checked != nil {
		*r.checked = append(*r.checked, workload)
	}
	return r.checkErr
}

func (r limitRuntime) HostCapStep(_ context.Context, want *gpuruntime.HostCapValues) (gpuruntime.HostCapStep, error) {
	if r.wanted != nil {
		*r.wanted = want
	}
	return r.step, r.stepErr
}

// fullLimits are limits that the runtime can launch: memory, tasks and
// CPU are set.
func fullLimits() control.ResourceLimits {
	return control.ResourceLimits{Version: 1, MemoryHighMiB: 4096, MemoryMaxMiB: 8192, TasksMax: 4096, CPUQuotaPercent: 400}
}

// limitedRequest gives a request with one owned unit with limits.
func limitedRequest(t *testing.T, home string) (Request, control.WorkloadProfile) {
	t.Helper()
	profile, _ := ownedFixtureProfile(t, home, "vision", 9100)
	plain := profile
	profile.NativeModel = new(control.NativeModel)
	*profile.NativeModel = *plain.NativeModel
	owned := *plain.NativeModel.Owned
	owned.Limits = fullLimits()
	profile.NativeModel.Owned = &owned
	refingerprint(t, &profile)
	request := ownedFixtureRequest(t, home, profile)
	request.Catalog.Version = control.CatalogVersionLimits
	request.ConfirmQuiesced = true
	return request, plain
}

// Apply writes the limits after the setup checks pass, and a later apply
// without them writes the lowest catalog version and the plain unit again.
func TestApplyWritesUnitLimits(t *testing.T) {
	backend, home, _ := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	var checked []control.Workload
	backend.makeRuntime = func(Request) (gpuruntime.Manager, error) { return limitRuntime{checked: &checked}, nil }
	request, plain := limitedRequest(t, home)
	ctx := context.Background()
	if err := backend.Apply(ctx, home, request); err != nil {
		t.Fatal(err)
	}
	accepted := acceptedRevision(t, request)
	unit, err := os.ReadFile(filepath.Join(ownedUnitDirectory(home), plain.Unit))
	if err != nil || accepted.Catalog.Version != control.CatalogVersionLimits || !accepted.Catalog.HasLimits() ||
		!bytes.Contains(unit, []byte("MemoryMax=8192M\n")) || len(checked) == 0 || checked[0] != "vision" {
		t.Fatalf("applied catalog version %d, limits %v, checked %v: %v", accepted.Catalog.Version, accepted.Catalog.HasLimits(), checked, err)
	}
	request.Catalog = control.Catalog{Version: control.CatalogVersionLimits, Profiles: []control.WorkloadProfile{plain}}
	request.ExpectedRevision = accepted.Revision
	if err := backend.Apply(ctx, home, request); err != nil {
		t.Fatalf("apply that removes the limits: %v", err)
	}
	_, plainUnit := ownedFixtureProfile(t, home, "vision", 9100)
	unit, err = os.ReadFile(filepath.Join(ownedUnitDirectory(home), plain.Unit))
	if accepted := acceptedRevision(t, request).Catalog; accepted.Version != 2 || accepted.HasLimits() || err != nil || !bytes.Equal(unit, plainUnit) {
		t.Fatalf("catalog after removal: version %d, limits %v: %v", accepted.Version, accepted.HasLimits(), err)
	}
}

// Preview gives the host cap step of the runtime for the requested values.
// A request without limits has no step.
func TestPreviewReportsTheHostCapStep(t *testing.T) {
	backend, home, _ := fixture(t)
	var wanted *gpuruntime.HostCapValues
	step := gpuruntime.HostCapStep{Status: gpuruntime.HostCapAbsent, UID: 1000, Commands: []string{"sudo systemctl daemon-reload"}}
	backend.makeRuntime = func(Request) (gpuruntime.Manager, error) { return limitRuntime{step: step, wanted: &wanted}, nil }
	request, plain := limitedRequest(t, home)
	request.HostCap = &gpuruntime.HostCapValues{MemoryHighMiB: 16384, MemoryMaxMiB: 20480}
	preview, err := backend.Plan(home, request)
	if err != nil || preview.HostCap == nil || preview.HostCap.Status != gpuruntime.HostCapAbsent || wanted != request.HostCap {
		t.Fatalf("preview = %+v, %v", preview.HostCap, err)
	}
	backend.makeRuntime = func(Request) (gpuruntime.Manager, error) {
		return limitRuntime{stepErr: gpuruntime.ErrLimitReserve}, nil
	}
	if _, err := backend.Plan(home, request); !errors.Is(err, gpuruntime.ErrLimitReserve) {
		t.Fatalf("preview with a failed step = %v", err)
	}
	request.HostCap = nil
	request.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{plain}}
	if preview, err := backend.Plan(home, request); err != nil || preview.HostCap == nil || preview.HostCap.Status != gpuruntime.HostCapNotNeeded {
		t.Fatalf("preview without limits = %+v, %v", preview.HostCap, err)
	}
}

// The host cap values must hold the units with their reserve, and need a
// unit with limits. The stored reserve of the profile counts.
func TestValidateHostCapValues(t *testing.T) {
	home := t.TempDir()
	request, plain := limitedRequest(t, home)
	request.HostCap = &gpuruntime.HostCapValues{MemoryHighMiB: 16384, MemoryMaxMiB: 20480}
	if err := Validate(request); err != nil {
		t.Fatalf("host cap that fits = %v", err)
	}
	request.Profile.LimitReserve = &gpuruntime.LimitReserve{MemoryMiB: 12000, Tasks: 512, CPUPercent: 100, IOPercent: 10}
	if err := Validate(request); !errors.Is(err, gpuruntime.ErrLimitReserve) {
		t.Fatalf("host cap with a large stored reserve = %v", err)
	}
	request.Profile.LimitReserve = nil
	request.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{plain}}
	if err := Validate(request); !errors.Is(err, errHostCapWithoutLimits) {
		t.Fatalf("host cap without limits = %v", err)
	}
}

// Limits on a unit that the accepted catalog adopts are refused, also in
// the apply that converts it. A new or owned unit can get limits.
func TestLimitsNeedConversion(t *testing.T) {
	home := t.TempDir()
	request, plain := limitedRequest(t, home)
	adopted := plain
	adopted.NativeModel = nil
	external := control.WorkloadProfile{ID: "vision", Unit: plain.Unit}
	// A conversion gives the unit a new name, so an adopted unit with a
	// name that the operator chose matches by the workload ID.
	chosen := control.WorkloadProfile{ID: "vision", Unit: "llama.service"}
	// An adopted unit with the same name matches by the unit, whatever
	// its workload ID.
	sameUnit := control.WorkloadProfile{ID: "legacy", Unit: plain.Unit}
	for name, c := range map[string]struct {
		accepted control.WorkloadProfile
		unit     string
	}{"adopted native": {adopted, plain.Unit}, "external": {external, plain.Unit}, "chosen unit name": {chosen, "llama.service"}, "same unit": {sameUnit, plain.Unit}} {
		accepted := control.Catalog{Profiles: []control.WorkloadProfile{c.accepted}}
		err := refuseLimitConversion(accepted, request.Catalog)
		if !errors.Is(err, ErrLimitsNeedConversion) || !strings.Contains(err.Error(), "vision (unit "+c.unit+") is adopted; convert it to a managed unit") {
			t.Errorf("%s: %v", name, err)
		}
		if err := (Backend{}).acceptLimits(context.Background(), request, accepted); !errors.Is(err, ErrLimitsNeedConversion) {
			t.Errorf("%s: apply = %v", name, err)
		}
	}
	other := control.WorkloadProfile{ID: "text", Unit: "llama.service"}
	for name, accepted := range map[string]control.Catalog{"none": {}, "owned": {Profiles: []control.WorkloadProfile{plain}}, "other adopted": {Profiles: []control.WorkloadProfile{other}}} {
		if err := refuseLimitConversion(accepted, request.Catalog); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Discovery reports the host cap step of the accepted catalog. Without
// limits, no root step is needed.
func TestDiscoveryReportsTheHostCapStep(t *testing.T) {
	backend, home, _ := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	result := Discovery{}
	backend.discoverHostCap(context.Background(), &result)
	if result.HostCap == nil || result.HostCap.Status != gpuruntime.HostCapNotNeeded || result.HostCap.Reserve != gpuruntime.DefaultLimitReserve {
		t.Fatalf("host cap without limits = %+v", result.HostCap)
	}
	stored := gpuruntime.LimitReserve{MemoryMiB: 4096, Tasks: 1024, CPUPercent: 200, IOPercent: 20}
	result = Discovery{Request: Request{Profile: Profile{LimitReserve: &stored}}}
	backend.discoverHostCap(context.Background(), &result)
	if result.HostCap == nil || result.HostCap.Reserve != stored {
		t.Fatalf("host cap with a stored reserve = %+v", result.HostCap)
	}
	request, _ := limitedRequest(t, home)
	step := gpuruntime.HostCapStep{Status: gpuruntime.HostCapPresent}
	backend.makeRuntime = func(Request) (gpuruntime.Manager, error) { return limitRuntime{step: step}, nil }
	result = Discovery{Request: request}
	backend.discoverHostCap(context.Background(), &result)
	if result.HostCap == nil || result.HostCap.Status != gpuruntime.HostCapPresent || len(result.Errors) != 0 {
		t.Fatalf("host cap with limits = %+v, %v", result.HostCap, result.Errors)
	}
	backend.makeRuntime = func(Request) (gpuruntime.Manager, error) { return idleRuntime{}, nil }
	result = Discovery{Request: request}
	backend.discoverHostCap(context.Background(), &result)
	if result.HostCap != nil || len(result.Errors) != 1 || !strings.Contains(result.Errors[0], "cannot be checked") {
		t.Fatalf("host cap without checks = %+v, %v", result.HostCap, result.Errors)
	}
	backend.makeRuntime = func(Request) (gpuruntime.Manager, error) { return nil, errors.New("no runtime") }
	result = Discovery{Request: request}
	backend.discoverHostCap(context.Background(), &result)
	if len(result.Errors) != 1 || !strings.Contains(result.Errors[0], "no runtime") {
		t.Fatalf("host cap without a runtime = %v", result.Errors)
	}
}

// With two units with limits, a failed check of the second one refuses
// the apply before any unit write, and the error names that unit.
func TestApplyChecksEachLimitedUnit(t *testing.T) {
	backend, home, _ := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	request, _ := limitedRequest(t, home)
	second, _ := ownedFixtureProfile(t, home, "audio", 9101)
	owned := *second.NativeModel.Owned
	owned.Limits = fullLimits()
	model := *second.NativeModel
	model.Owned = &owned
	model.Instance = "second"
	second.NativeModel = &model
	refingerprint(t, &second)
	request.Catalog.Profiles = append(request.Catalog.Profiles, second)
	var checked []control.Workload
	backend.makeRuntime = func(Request) (gpuruntime.Manager, error) {
		return failSecond{limitRuntime: limitRuntime{checked: &checked}}, nil
	}
	err := backend.Apply(context.Background(), home, request)
	if !errors.Is(err, gpuruntime.ErrHostCap) || !strings.Contains(err.Error(), "audio (") || len(checked) != 2 {
		t.Fatalf("apply with a failed second check = %v, checked %v", err, checked)
	}
	for _, p := range request.Catalog.Profiles {
		if _, err := os.Stat(p.NativeModel.LaunchFile); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unit %s written for a refused apply: %v", p.Unit, err)
		}
	}
}

// failSecond fails the host check of the second checked unit.
type failSecond struct{ limitRuntime }

func (r failSecond) CheckLimitHost(ctx context.Context, workload control.Workload) error {
	_ = r.limitRuntime.CheckLimitHost(ctx, workload)
	if len(*r.checked) == 2 {
		return gpuruntime.ErrHostCap
	}
	return nil
}

// Apply reads the accepted catalog for the conversion refusal: after an
// apply that adopts the vision workload, an apply that converts it and
// adds limits is refused before any unit write.
func TestApplyRefusesLimitsOnAnAcceptedAdoptedUnit(t *testing.T) {
	backend, home, r := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	adopted := control.WorkloadProfile{ID: "vision", Label: "Vision", Adapter: "systemd", Unit: "vision.service", Cgroup: "/user.slice/vision",
		HealthURL: "http://127.0.0.1:8001/health", BootPolicy: "stop-to-idle"}
	r.Catalog = control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{adopted}}
	ctx := context.Background()
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	request, plain := limitedRequest(t, home)
	request.Profile = r.Profile
	request.ExpectedRevision = acceptedRevision(t, r).Revision
	var checked []control.Workload
	backend.makeRuntime = func(Request) (gpuruntime.Manager, error) { return limitRuntime{checked: &checked}, nil }
	err := backend.Apply(ctx, home, request)
	if !errors.Is(err, ErrLimitsNeedConversion) || !strings.Contains(err.Error(), "unit vision.service") || len(checked) != 0 {
		t.Fatalf("apply that converts and limits = %v, checked %v", err, checked)
	}
	if _, err := os.Stat(filepath.Join(ownedUnitDirectory(home), plain.Unit)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unit written for a refused apply: %v", err)
	}
}

// The boot reconcile and the policy tick read operator.json again under the
// state lock. A profile that changed in between is refused.
func TestSameProfileUnderTheLock(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config/gpu-workload-supervisor")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := sameProfile(home, nil); err == nil {
		t.Fatal("a missing profile passed")
	}
	if err := os.WriteFile(filepath.Join(dir, "operator.json"), []byte(`{"version":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := sameProfile(home, []byte(`{"version":1}`)); err != nil {
		t.Fatalf("unchanged profile = %v", err)
	}
	if err := sameProfile(home, []byte(`{"version":1,"limitReserve":{}}`)); !errors.Is(err, errProfileChanged) {
		t.Fatalf("changed profile = %v", err)
	}
}
