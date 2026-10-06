package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
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
	if err := stateStore.FinishWorkFenced(ctx, "request-1", control.WorkloadMedia, stale, WorkCompleted); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("complete with stale fence: %v", err)
	}
	if err := stateStore.FinishWorkFenced(ctx, "request-1", control.WorkloadMedia, state.LeaseFence, WorkCompleted); err != nil {
		t.Fatal(err)
	}
	if err := stateStore.FinishWorkFenced(ctx, "request-1", control.WorkloadMedia, state.LeaseFence, WorkCompleted); !errors.Is(err, sql.ErrNoRows) {
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
	if err := stateStore.FinishWorkFenced(ctx, "request-1", control.WorkloadMedia, registeredFence, WorkCompleted); err != nil {
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

func TestFinishWorkRejectsDifferentWorkload(t *testing.T) {
	stateStore := testStore(t)
	ctx := context.Background()
	state, err := stateStore.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state.DesiredWorkload = control.WorkloadText
	state.ActiveWorkload = control.WorkloadText
	state.Phase = control.PhaseStable
	state.Health = control.HealthHealthy
	state.Admission = control.AdmissionOpen
	state, err = stateStore.UpdateState(ctx, state.Version, state)
	if err != nil {
		t.Fatal(err)
	}
	if err := stateStore.AdmitWork(ctx, "request-1", "", control.WorkloadText, state.LeaseFence); err != nil {
		t.Fatal(err)
	}
	if err := stateStore.FinishWorkFenced(ctx, "request-1", control.WorkloadMedia, state.LeaseFence, WorkAbandoned); !errors.Is(err, ErrWorkloadMismatch) {
		t.Fatalf("finish different workload: %v", err)
	}
}

func TestAdmitWorkRejectsInvalidFence(t *testing.T) {
	stateStore := testStore(t)
	ctx := context.Background()
	err := stateStore.AdmitWork(ctx, "request-1", "", control.WorkloadMedia, control.Fence{})
	if err == nil || !strings.Contains(err.Error(), "invalid fence") {
		t.Fatalf("admit with empty fence: %v", err)
	}
}

func TestFinishWorkLegacyNullWorkload(t *testing.T) {
	stateStore := testStore(t)
	ctx := context.Background()
	state, err := stateStore.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stateStore.db.ExecContext(ctx, `INSERT INTO registered_work
		(request_id, job_id, workload, lease_incarnation, lease_epoch, registered_at)
		VALUES ('legacy-1', NULL, NULL, ?, ?, ?)`,
		state.LeaseFence.Incarnation, state.LeaseFence.Epoch, formatTime(stateStore.now())); err != nil {
		t.Fatal(err)
	}
	if err := stateStore.FinishWorkFenced(ctx, "legacy-1", control.WorkloadText, state.LeaseFence, WorkAbandoned); !errors.Is(err, ErrWorkloadMismatch) {
		t.Fatalf("finish legacy null workload: %v", err)
	}
}
