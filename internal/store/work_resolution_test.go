package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func admittedResolutionWork(t *testing.T, stateStore *Store) control.State {
	t.Helper()
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
	for _, requestID := range []string{"orphan-1", "orphan-2"} {
		if _, err := stateStore.AdmitWorkToken(ctx, requestID, "", control.WorkloadMedia, state.LeaseFence); err != nil {
			t.Fatal(err)
		}
	}
	return state
}

func TestResolveUnfinishedWorkRequiresClosedRotatedFenceAndAudits(t *testing.T) {
	ctx := context.Background()
	stateStore := testStore(t)
	opened := admittedResolutionWork(t, stateStore)
	if _, err := stateStore.ResolveUnfinishedWork(ctx, opened.Version, "incident-123"); !errors.Is(err, ErrAdmissionOpen) {
		t.Fatalf("open admission accepted: %v", err)
	}
	closed := opened
	closed.Admission = control.AdmissionClosed
	closed, err := stateStore.UpdateState(ctx, opened.Version, closed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stateStore.ResolveUnfinishedWork(ctx, closed.Version, "incident-123"); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("unrotated fence accepted: %v", err)
	}
	rotated, err := stateStore.RotateFenceAndCloseAdmission(ctx, closed.Version)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stateStore.ResolveUnfinishedWork(ctx, closed.Version, "incident-123"); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale state version accepted: %v", err)
	}
	for _, reason := range []string{" ", strings.Repeat("x", 513)} {
		if _, err := stateStore.ResolveUnfinishedWork(ctx, rotated.Version, reason); err == nil {
			t.Fatalf("invalid reason %q accepted", reason)
		}
	}
	count, err := stateStore.ResolveUnfinishedWork(ctx, rotated.Version, "  incident-123  ")
	if err != nil || count != 2 {
		t.Fatalf("resolution count=%d error=%v", count, err)
	}
	var remaining, audits int
	var reason, incarnation string
	var epoch, version, auditedCount int64
	if err := stateStore.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM registered_work WHERE completed_at IS NULL`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if err := stateStore.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM work_resolutions`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if err := stateStore.db.QueryRowContext(ctx, `SELECT reason, lease_incarnation, lease_epoch, state_version, abandoned_work FROM work_resolutions`).Scan(&reason, &incarnation, &epoch, &version, &auditedCount); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 || audits != 1 || reason != "incident-123" ||
		incarnation != rotated.LeaseFence.Incarnation || epoch != int64(rotated.LeaseFence.Epoch) ||
		version != int64(rotated.Version) || auditedCount != 2 {
		t.Fatalf("remaining=%d audits=%d reason=%q fence=%s/%d version=%d count=%d", remaining, audits, reason, incarnation, epoch, version, auditedCount)
	}
	if _, err := stateStore.ResolveUnfinishedWork(ctx, rotated.Version, "again"); !errors.Is(err, ErrNoUnfinishedWork) {
		t.Fatalf("empty repeat resolution error = %v", err)
	}
}

func TestResolveUnfinishedWorkAuditFailureRollsBackCompletions(t *testing.T) {
	ctx := context.Background()
	stateStore := testStore(t)
	opened := admittedResolutionWork(t, stateStore)
	rotated, err := stateStore.RotateFenceAndCloseAdmission(ctx, opened.Version)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stateStore.db.ExecContext(ctx, `CREATE TRIGGER reject_work_resolution BEFORE INSERT ON work_resolutions
		BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := stateStore.ResolveUnfinishedWork(ctx, rotated.Version, "incident-456"); err == nil || !strings.Contains(err.Error(), "audit unavailable") {
		t.Fatalf("audit failure = %v", err)
	}
	var pending int
	if err := stateStore.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM registered_work WHERE completed_at IS NULL`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 2 {
		t.Fatalf("audit failure abandoned %d unfinished rows", 2-pending)
	}
}

func TestResolveUnfinishedWorkRejectsUserOwnership(t *testing.T) {
	ctx := context.Background()
	stateStore := testStore(t)
	opened := admittedResolutionWork(t, stateStore)
	rotated, err := stateStore.RotateFenceAndCloseAdmission(ctx, opened.Version)
	if err != nil {
		t.Fatal(err)
	}
	rotated.Owner = control.OwnerUser
	userState, err := stateStore.UpdateState(ctx, rotated.Version, rotated)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stateStore.ResolveUnfinishedWork(ctx, userState.Version, "incident-789"); err == nil || !strings.Contains(err.Error(), "supervisor ownership") {
		t.Fatalf("user-owned resolution error = %v", err)
	}
}
