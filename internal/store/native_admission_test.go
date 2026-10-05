package store

import (
	"context"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"testing"
)

func TestNativeAdmissionPinsCatalogTransaction(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state.DesiredWorkload = control.WorkloadMedia
	state.ActiveWorkload = control.WorkloadMedia
	state.Phase = control.PhaseStable
	state.Health = control.HealthHealthy
	state.Admission = control.AdmissionOpen
	state, err = s.UpdateState(ctx, state.Version, state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdmitWorkTokenAtCatalog(ctx, "stale", "", control.WorkloadMedia, state.LeaseFence, "changed"); !errors.Is(err, ErrVersionConflict) {
		t.Fatal(err)
	}
	if count, err := s.PendingWork(ctx); err != nil || count != 0 {
		t.Fatal("stale admission created work", count, err)
	}
	if token, err := s.AdmitWorkTokenAtCatalog(ctx, "current", "", control.WorkloadMedia, state.LeaseFence, ""); err != nil || token == "" {
		t.Fatal(err)
	}
}
