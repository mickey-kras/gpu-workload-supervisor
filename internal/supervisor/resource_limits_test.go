package supervisor

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

func limitedCatalog() control.Catalog {
	catalog := testCatalog()
	catalog.Version = control.CatalogVersionLimits
	catalog.Profiles = append(catalog.Profiles, control.WorkloadProfile{
		ID: "vision", Label: "Vision", Adapter: "systemd", Unit: "gws-owned-vision.service",
		Cgroup:    "/user.slice/user-1000.slice/user@1000.service/app.slice/gws-owned-vision.service",
		HealthURL: "http://127.0.0.1:9102/health",
		NativeModel: &control.NativeModel{
			Runtime: "vllm", Instance: "local", Model: "vision-q8", Endpoint: "http://127.0.0.1:9102",
			LaunchFile:   "/home/u/.config/systemd/user/gws-owned-vision.service",
			LaunchSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Owned: &control.OwnedLaunch{ModelPath: "/models/vision-q8", Port: 9102, Alias: "vision-q8",
				Limits: control.ResourceLimits{Version: 1, TasksMax: 4096}},
		},
	})
	return catalog
}

// limitedRuntime runs the vision unit from limitedCatalog next to the
// fake text and media units. Its runtime gate refuses vision as the real
// runtime does.
type limitedRuntime struct {
	fakeRuntime
	visionActive bool
	profile      control.WorkloadProfile
}

func (r *limitedRuntime) Observe(ctx context.Context) (gpuruntime.Snapshot, error) {
	snapshot, err := r.fakeRuntime.Observe(ctx)
	if err == nil {
		snapshot.Workloads["vision"] = gpuruntime.WorkloadObservation{Active: r.visionActive, Exclusive: true}
	}
	return snapshot, err
}

func (r *limitedRuntime) Start(ctx context.Context, w control.Workload) error {
	if w == "vision" {
		r.calls = append(r.calls, "start vision")
		return gpuruntime.RequireEnforceableLimits(r.profile)
	}
	return r.fakeRuntime.Start(ctx, w)
}

func (r *limitedRuntime) Stop(ctx context.Context, w control.Workload) error {
	if w == "vision" {
		r.calls = append(r.calls, "stop vision")
		r.visionActive = false
		return nil
	}
	return r.fakeRuntime.Stop(ctx, w)
}

func (r *limitedRuntime) Healthy(ctx context.Context, w control.Workload) error {
	if w == "vision" {
		return gpuruntime.RequireEnforceableLimits(r.profile)
	}
	return r.fakeRuntime.Healthy(ctx, w)
}

func newLimitedRuntime(catalog control.Catalog) *limitedRuntime {
	p, _ := catalog.Profile("vision")
	return &limitedRuntime{profile: p}
}

func TestSwitchRefusesLimitedTargetBeforeAnyEffect(t *testing.T) {
	ctx := context.Background()
	s := openStoreAt(t, filepath.Join(t.TempDir(), "state.db"))
	if _, err := s.ReplaceCatalog(ctx, "", limitedCatalog()); err != nil {
		t.Fatal(err)
	}
	runtime := &fakeRuntime{active: control.WorkloadText, mediaReady: true}
	c := testController(t, s, runtime)
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	runtime.calls = nil
	if _, err := c.Switch(ctx, "vision", "test"); !errors.Is(err, ErrLimitedTarget) || !errors.Is(err, gpuruntime.ErrLimitsNotEnforced) {
		t.Fatalf("switch to a limited target = %v", err)
	}
	after, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) || after.ActiveWorkload != control.WorkloadText || after.Health != before.Health {
		t.Fatalf("refused switch changed state: before=%+v after=%+v", before, after)
	}
	if _, err := c.ActivateWorkload(ctx, "vision", control.OperatorPrecondition{Owner: control.OwnerSupervisor}); !errors.Is(err, gpuruntime.ErrLimitsNotEnforced) {
		t.Fatalf("activation of a limited target = %v", err)
	}
	if len(runtime.calls) != 0 {
		t.Fatalf("refused switch called the runtime: %v", runtime.calls)
	}
	// A limited profile does not block other targets.
	if state, err := c.Switch(ctx, control.WorkloadMedia, "test"); err != nil || state.ActiveWorkload != control.WorkloadMedia {
		t.Fatalf("switch to an unlimited target = %+v, %v", state, err)
	}
}

func TestStaleCatalogWinsOverLimitedRefusal(t *testing.T) {
	ctx := context.Background()
	s := openStoreAt(t, filepath.Join(t.TempDir(), "state.db"))
	snap, err := s.ReplaceCatalog(ctx, "", limitedCatalog())
	if err != nil {
		t.Fatal(err)
	}
	c := testController(t, s, &fakeRuntime{})
	// Setup removed the limits; this controller still holds the old catalog.
	plain := limitedCatalog()
	plain.Profiles[2].NativeModel.Owned.Limits = control.ResourceLimits{}
	plain.Version = control.LowestCatalogVersion(plain)
	if _, err := s.ReplaceCatalog(ctx, snap.Revision, plain); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Switch(ctx, "vision", "test"); !errors.Is(err, store.ErrVersionConflict) {
		t.Fatalf("switch with a stale catalog = %v", err)
	}
	if _, err := c.ActivateWorkload(ctx, "vision", control.OperatorPrecondition{Owner: control.OwnerSupervisor}); !errors.Is(err, store.ErrVersionConflict) {
		t.Fatalf("activation with a stale catalog = %v", err)
	}
}

func TestReconcileStopsRetainedLimitedUnit(t *testing.T) {
	ctx := context.Background()
	catalog := limitedCatalog()
	catalog.Profiles[2].BootPolicy = "retain"
	s := openStoreAt(t, filepath.Join(t.TempDir(), "state.db"))
	if _, err := s.ReplaceCatalog(ctx, "", catalog); err != nil {
		t.Fatal(err)
	}
	r := newLimitedRuntime(catalog)
	r.visionActive = true
	c := testController(t, s, r)
	state, err := c.Reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile with a retained limited unit = %v", err)
	}
	if state.ActiveWorkload != control.WorkloadIdle || state.Health != control.HealthHealthy || r.visionActive {
		t.Fatalf("limited unit retained: %+v, active %v", state, r.visionActive)
	}
}

func TestLimitedBackstopAfterEffectsIsNotARefusal(t *testing.T) {
	ctx := context.Background()
	catalog := limitedCatalog()
	for name, act := range map[string]func(*Controller) error{
		// Status latches the gate when the active limited unit is not healthy.
		"status latch": func(c *Controller) error { _, err := c.Status(ctx); return err },
		// Rollback to the limited unit after a failed switch.
		"rollback": func(c *Controller) error { _, err := c.Switch(ctx, control.WorkloadMedia, "test"); return err },
	} {
		t.Run(name, func(t *testing.T) {
			s := openStoreAt(t, filepath.Join(t.TempDir(), "state.db"))
			if _, err := s.ReplaceCatalog(ctx, "", catalog); err != nil {
				t.Fatal(err)
			}
			ownershipState(t, s, control.OwnerSupervisor, "vision")
			r := newLimitedRuntime(catalog)
			r.visionActive = true
			r.startErr = errors.New("media failed")
			c := testController(t, s, r)
			err := act(c)
			if !errors.Is(err, gpuruntime.ErrLimitsNotEnforced) || errors.Is(err, ErrLimitedTarget) {
				t.Fatalf("%s error = %v", name, err)
			}
			after, stateErr := s.State(ctx)
			if stateErr != nil || after.Health != control.HealthError {
				t.Fatalf("%s did not latch: %+v, %v", name, after, stateErr)
			}
		})
	}
}
