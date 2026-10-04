package store

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestPendingWorkIncludesOldFencesUntilEveryRegistrationFinishes(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	assertPending := func(want int) {
		t.Helper()
		got, err := s.PendingWork(ctx)
		if err != nil || got != want {
			t.Fatalf("PendingWork = %d, %v; want %d, nil", got, err, want)
		}
	}
	assertPending(0)
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
	tokens := make(map[string]string)
	for _, id := range []string{"completed", "abandoned"} {
		tokens[id], err = s.AdmitWorkToken(ctx, id, "", control.WorkloadMedia, state.LeaseFence)
		if err != nil {
			t.Fatal(err)
		}
	}
	// This is an existence probe, not a count, and must include pre-recovery fences.
	assertPending(1)
	if _, err := s.RotateFenceAndCloseAdmission(ctx, state.Version); err != nil {
		t.Fatal(err)
	}
	assertPending(1)
	if err := s.FinishWorkToken(ctx, "completed", control.WorkloadMedia, state.LeaseFence, tokens["completed"], WorkCompleted); err != nil {
		t.Fatal(err)
	}
	assertPending(1)
	if err := s.FinishWorkToken(ctx, "abandoned", control.WorkloadMedia, state.LeaseFence, tokens["abandoned"], WorkAbandoned); err != nil {
		t.Fatal(err)
	}
	assertPending(0)
}

func TestPendingWorkPropagatesUnavailableDatabase(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.PendingWork(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled PendingWork: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PendingWork(context.Background()); err == nil {
		t.Fatal("closed database reported a successful idle result")
	}
}

func TestRejectedCompletionPreservesPendingWork(t *testing.T) {
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
	token, err := s.AdmitWorkToken(ctx, "pending", "", control.WorkloadMedia, state.LeaseFence)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, id string
		workload control.Workload
		fence    control.Fence
		outcome  WorkOutcome
	}{
		{"empty request", "", control.WorkloadMedia, state.LeaseFence, WorkCompleted},
		{"invalid fence", "pending", control.WorkloadMedia, control.Fence{}, WorkCompleted},
		{"invalid workload", "pending", control.Workload("invalid"), state.LeaseFence, WorkCompleted},
		{"invalid outcome", "pending", control.WorkloadMedia, state.LeaseFence, WorkOutcome("invalid")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.FinishWorkToken(ctx, tc.id, tc.workload, tc.fence, token, tc.outcome); err == nil {
				t.Fatal("invalid completion accepted")
			}
			if pending, err := s.PendingWork(ctx); err != nil || pending != 1 {
				t.Fatalf("invalid completion changed recovery barrier: %d, %v", pending, err)
			}
		})
	}
	if err := s.FinishWorkToken(ctx, "pending", control.WorkloadMedia, state.LeaseFence, token, WorkCompleted); err != nil {
		t.Fatal(err)
	}
	if pending, err := s.PendingWork(ctx); err != nil || pending != 0 {
		t.Fatalf("valid completion did not release recovery barrier: %d, %v", pending, err)
	}
}

func TestPendingWorkExceptIncludesOpposingAndUnknownRegistrations(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	state, _ := s.State(ctx)
	for _, workload := range []any{"text", "media", nil} {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO registered_work(request_id,lease_incarnation,lease_epoch,registered_at,workload) VALUES(?,?,?,?,?)`, fmt.Sprint(workload), state.LeaseFence.Incarnation, state.LeaseFence.Epoch, formatTime(s.now()), workload); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.PendingWorkExcept(ctx, control.WorkloadText); err != nil || n != 1 {
		t.Fatalf("opposing %d %v", n, err)
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM registered_work WHERE workload='media'"); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PendingWorkExcept(ctx, control.WorkloadText); err != nil || n != 1 {
		t.Fatalf("unknown %d %v", n, err)
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM registered_work WHERE workload IS NULL"); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PendingWorkExcept(ctx, control.WorkloadText); err != nil || n != 0 {
		t.Fatalf("retained %d %v", n, err)
	}
}
