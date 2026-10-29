package supervisor

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
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
	if _, err := c.Switch(ctx, "vision", "test"); !errors.Is(err, gpuruntime.ErrLimitsNotEnforced) {
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
