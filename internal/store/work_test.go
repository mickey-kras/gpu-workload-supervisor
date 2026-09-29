package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestCompleteWorkFenced(t *testing.T) {
	stateStore := testStore(t)
	ctx := context.Background()
	state, err := stateStore.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state.DesiredWorkload = control.WorkloadMedia
	state.ActiveWorkload = control.WorkloadMedia
	state.Phase = control.PhaseStable
	state.Health = control.HealthHealthy
	state.Admission = control.AdmissionOpen
	state, err = stateStore.UpdateState(ctx, state.Version, state)
	if err != nil {
		t.Fatal(err)
	}
	if err := stateStore.AdmitWork(ctx, "request-1", "job-1", control.WorkloadMedia, state.LeaseFence); err != nil {
		t.Fatal(err)
	}
	stale := state.LeaseFence
	stale.Epoch++
	if err := stateStore.CompleteWorkFenced(ctx, "request-1", stale); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("complete with stale fence: %v", err)
	}
	if err := stateStore.CompleteWorkFenced(ctx, "request-1", state.LeaseFence); err != nil {
		t.Fatal(err)
	}
	if err := stateStore.CompleteWorkFenced(ctx, "request-1", state.LeaseFence); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("complete twice: %v", err)
	}
}

func TestCompletionAllowsRegisteredFenceAfterRotation(t *testing.T) {
	stateStore := testStore(t)
	ctx := context.Background()
	state, err := stateStore.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state.DesiredWorkload = control.WorkloadMedia
	state.ActiveWorkload = control.WorkloadMedia
	state.Phase = control.PhaseStable
	state.Health = control.HealthHealthy
	state.Admission = control.AdmissionOpen
	state, err = stateStore.UpdateState(ctx, state.Version, state)
	if err != nil {
		t.Fatal(err)
	}
	registeredFence := state.LeaseFence
	if err := stateStore.AdmitWork(ctx, "request-1", "", control.WorkloadMedia, registeredFence); err != nil {
		t.Fatal(err)
	}
	if _, err := stateStore.RotateFenceAndCloseAdmission(ctx, state.Version); err != nil {
		t.Fatal(err)
	}
	if err := stateStore.CompleteWorkFenced(ctx, "request-1", registeredFence); err != nil {
		t.Fatalf("complete registered fence after rotation: %v", err)
	}
}

func TestAdmitWorkRejectsDuplicateRequestID(t *testing.T) {
	stateStore := testStore(t)
	ctx := context.Background()
	state, err := stateStore.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state.DesiredWorkload = control.WorkloadMedia
	state.ActiveWorkload = control.WorkloadMedia
	state.Phase = control.PhaseStable
	state.Health = control.HealthHealthy
	state.Admission = control.AdmissionOpen
	state, err = stateStore.UpdateState(ctx, state.Version, state)
	if err != nil {
		t.Fatal(err)
	}
	if err := stateStore.AdmitWork(ctx, "request-1", "", control.WorkloadMedia, state.LeaseFence); err != nil {
		t.Fatal(err)
	}
	if err := stateStore.AdmitWork(ctx, "request-1", "", control.WorkloadMedia, state.LeaseFence); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("duplicate request: %v", err)
	}
}
