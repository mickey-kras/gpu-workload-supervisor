package supervisor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

func ownedSupervisorCatalog() control.Catalog {
	return control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{{
		ID:        "vision",
		Label:     "Vision",
		Adapter:   "systemd",
		Unit:      "gws-owned-vision.service",
		Cgroup:    "/user.slice/user-1000.slice/user@1000.service/app.slice/gws-owned-vision.service",
		HealthURL: "http://127.0.0.1:9100/health",
		NativeModel: &control.NativeModel{
			Runtime:      "llama.cpp",
			Instance:     "owned",
			Model:        "vision-q8",
			Endpoint:     "http://127.0.0.1:9100",
			LaunchFile:   "/home/u/.config/systemd/user/gws-owned-vision.service",
			LaunchSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Owned:        &control.OwnedLaunch{ModelPath: "/models/vision-q8.gguf", Port: 9100},
		},
	}}}
}

func TestSupervisorAcceptsV2OwnedCatalog(t *testing.T) {
	s := openStore(t)
	snapshot := control.CatalogSnapshot{Revision: "r1", Catalog: ownedSupervisorCatalog()}
	c, err := New(s, &fakeRuntime{}, Config{
		Catalog:      &snapshot,
		DrainTimeout: time.Second, VerifyTimeout: time.Second,
		ActionTimeout: time.Second, CleanupTimeout: time.Second,
		FinalizeTimeout: time.Second, PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("v2 owned catalog rejected: %v", err)
	}
	if c == nil {
		t.Fatal("no controller")
	}
}

func TestOrphanedOwnedUnitLatchesRecovery(t *testing.T) {
	s := openStore(t)
	before := ownershipState(t, s, control.OwnerSupervisor, control.WorkloadText)
	r := &fakeRuntime{active: control.WorkloadText}
	c := testController(t, s, preflightRuntime{r, gpuruntime.ErrOrphanedOwnedUnit})
	_, err := c.Recover(context.Background())
	if !errors.Is(err, gpuruntime.ErrOrphanedOwnedUnit) {
		t.Fatalf("orphan preflight = %v", err)
	}
	after, _ := s.State(context.Background())
	if after.Admission != control.AdmissionClosed || after.LeaseFence == before.LeaseFence || len(r.calls) != 0 {
		t.Fatalf("orphan recovery not latched closed: %#v calls %v", after, r.calls)
	}
}
