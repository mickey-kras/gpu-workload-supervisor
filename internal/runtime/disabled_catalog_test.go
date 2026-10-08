package runtime

import (
	"context"
	"net/http"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestDisabledCatalogCannotStartOrStopFormerWorkloads(t *testing.T) {
	cfg := testConfig()
	cfg.Catalog = &control.Catalog{Version: 1, Disabled: true}
	// Keep operator headroom preferences when the final measured target is removed.
	cfg.CapacityHeadroomMiB = 1024
	r := &fakeRunner{}
	m, err := newSystemdManager(cfg, r, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	snapshot, err := m.Observe(ctx)
	if err != nil || len(snapshot.Workloads) != 0 {
		t.Fatalf("disabled observation: %+v %v", snapshot, err)
	}
	if err := m.ReleasedFor(ctx, control.WorkloadIdle); err != nil {
		t.Fatal(err)
	}
	if err := m.StopForRecovery(ctx); err != nil {
		t.Fatal(err)
	}
	for _, action := range []func(context.Context, control.Workload) error{m.Start, m.Stop, m.Healthy, m.ReleasedFor} {
		if err := action(ctx, control.WorkloadText); err == nil {
			t.Fatal("former workload accepted")
		}
	}
	if len(r.calls) != 0 {
		t.Fatalf("disabled catalog controlled former units: %v", r.calls)
	}
}
