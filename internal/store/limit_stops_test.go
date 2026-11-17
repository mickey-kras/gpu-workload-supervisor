package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

// The record keeps the first failure and the first definite failure of
// one workload. A record of another workload with no started stop
// replaces it. The stop time comes last, also with a clock that goes back.
func TestLimitStopRecordKeepsFirstTimes(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(d time.Duration) time.Time { return base.Add(d) }
	if _, ok, err := s.LimitStop(ctx); ok || err != nil {
		t.Fatalf("empty record = %v, %v", ok, err)
	}
	want := func(got LimitStop, err error, w LimitStop) {
		t.Helper()
		if err != nil || got != w {
			t.Fatalf("record = %+v, %v; want %+v", got, err, w)
		}
	}
	got, err := s.MarkLimitFailure(ctx, "vision", false, at(0))
	want(got, err, LimitStop{Workload: "vision", FirstSeenAt: at(0)})
	got, err = s.MarkLimitFailure(ctx, "vision", true, at(time.Minute))
	want(got, err, LimitStop{Workload: "vision", FirstSeenAt: at(0), DefiniteAt: at(time.Minute)})
	got, err = s.MarkLimitFailure(ctx, "vision", true, at(2*time.Minute))
	want(got, err, LimitStop{Workload: "vision", FirstSeenAt: at(0), DefiniteAt: at(time.Minute)})
	if err := s.MarkLimitStopping(ctx, "text", at(3*time.Minute)); !errors.Is(err, ErrNoLimitFailure) {
		t.Fatalf("stop of another workload = %v", err)
	}
	// A clock that goes back still keeps the order of the times.
	if err := s.MarkLimitStopping(ctx, "vision", at(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	got, _, err = s.LimitStop(ctx)
	want(got, err, LimitStop{Workload: "vision", FirstSeenAt: at(0), DefiniteAt: at(time.Minute), StoppingAt: at(time.Minute)})
	// Another workload takes a record with no started stop, and keeps the
	// earlier first time.
	if err := s.ClearLimitStop(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkLimitFailure(ctx, "vision", false, at(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, err = s.MarkLimitFailure(ctx, "text", false, at(4*time.Minute))
	want(got, err, LimitStop{Workload: "text", FirstSeenAt: at(3 * time.Minute)})
	// An overdue failure that proves nothing gets its stop time as its
	// definite time.
	if err := s.MarkLimitStopping(ctx, "text", at(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, _, err = s.LimitStop(ctx)
	want(got, err, LimitStop{Workload: "text", FirstSeenAt: at(3 * time.Minute), DefiniteAt: at(5 * time.Minute), StoppingAt: at(5 * time.Minute)})
	if err := s.ClearLimitStop(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.LimitStop(ctx); ok || err != nil {
		t.Fatalf("cleared record = %v, %v", ok, err)
	}
}

// A closed database fails every limit stop call.
func TestLimitStopRecordFailsOnClosedDatabase(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.LimitStop(ctx); err == nil {
		t.Fatal("read of a closed database passed")
	}
	if _, err := s.MarkLimitFailure(ctx, "vision", true, time.Now()); err == nil {
		t.Fatal("failure record of a closed database passed")
	}
	if err := s.MarkLimitStopping(ctx, "vision", time.Now()); err == nil {
		t.Fatal("stop record of a closed database passed")
	}
	if err := s.ClearLimitStop(ctx); err == nil {
		t.Fatal("clear of a closed database passed")
	}
	if _, err := s.PendingWorkFor(ctx, "vision"); err == nil {
		t.Fatal("pending work of a closed database passed")
	}
}

// Pending work of one workload includes work without a workload identity.
func TestPendingWorkForCountsUnattributedWork(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state.DesiredWorkload, state.ActiveWorkload = control.WorkloadMedia, control.WorkloadMedia
	state.Phase, state.Health, state.Admission = control.PhaseStable, control.HealthHealthy, control.AdmissionOpen
	if state, err = s.UpdateState(ctx, state.Version, state); err != nil {
		t.Fatal(err)
	}
	pending := func(w control.Workload, want int) {
		t.Helper()
		if got, err := s.PendingWorkFor(ctx, w); err != nil || got != want {
			t.Fatalf("pending work of %s = %d, %v; want %d", w, got, err, want)
		}
	}
	pending(control.WorkloadMedia, 0)
	if _, err := s.AdmitWorkToken(ctx, "job", "", control.WorkloadMedia, state.LeaseFence); err != nil {
		t.Fatal(err)
	}
	pending(control.WorkloadMedia, 1)
	pending(control.WorkloadText, 0)
	if _, err := s.db.ExecContext(ctx, `UPDATE registered_work SET workload = NULL`); err != nil {
		t.Fatal(err)
	}
	pending(control.WorkloadText, 1)
}

// A definite failure with a clock that went back before the first failure
// keeps the order of the times.
func TestLimitFailureClampsBackwardClock(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := s.MarkLimitFailure(ctx, "vision", false, base); err != nil {
		t.Fatal(err)
	}
	got, err := s.MarkLimitFailure(ctx, "vision", true, base.Add(-time.Hour))
	if err != nil || got.DefiniteAt != base || got.FirstSeenAt != base {
		t.Fatalf("record = %+v, %v", got, err)
	}
}

// A state write that ends the latch removes the record in the same
// transaction. A write that keeps the latch keeps it.
func TestStateWriteEndingLatchClearsLimitStop(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state.Health, state.Admission, state.Phase = control.HealthError, control.AdmissionClosed, control.PhaseReconciling
	if state, err = s.UpdateState(ctx, state.Version, state); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkLimitFailure(ctx, "vision", true, time.Now()); err != nil {
		t.Fatal(err)
	}
	if state, err = s.UpdateState(ctx, state.Version, state); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.LimitStop(ctx); !ok || err != nil {
		t.Fatalf("record after a latched write = %v, %v", ok, err)
	}
	state.Health, state.Phase = control.HealthHealthy, control.PhaseStable
	if _, err = s.Recover(ctx, state.Version, state, "test"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.LimitStop(ctx); ok || err != nil {
		t.Fatalf("record after the latch ended = %v, %v", ok, err)
	}
}

// A started stop keeps its record: the same workload keeps the stop time,
// and another workload cannot replace the record.
func TestLimitFailureKeepsStartedStop(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := s.MarkLimitFailure(ctx, "vision", true, base); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkLimitStopping(ctx, "vision", base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	want := LimitStop{Workload: "vision", FirstSeenAt: base, DefiniteAt: base, StoppingAt: base.Add(time.Minute)}
	if got, err := s.MarkLimitFailure(ctx, "vision", true, base.Add(2*time.Minute)); err != nil || got != want {
		t.Fatalf("record = %+v, %v; want %+v", got, err, want)
	}
	if _, err := s.MarkLimitFailure(ctx, "text", false, base.Add(3*time.Minute)); !errors.Is(err, ErrLimitStopStarted) {
		t.Fatalf("record of another workload = %v", err)
	}
	if got, _, err := s.LimitStop(ctx); err != nil || got != want {
		t.Fatalf("record after the refusal = %+v, %v", got, err)
	}
}

// Units that fail in turn keep the first time and the definite time of
// the latch.
func TestLimitFailureOfAnotherUnitKeepsLatchTimes(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := s.MarkLimitFailure(ctx, "vision", true, base); err != nil {
		t.Fatal(err)
	}
	got, err := s.MarkLimitFailure(ctx, "audio", false, base.Add(time.Minute))
	if err != nil || got != (LimitStop{Workload: "audio", FirstSeenAt: base, DefiniteAt: base}) {
		t.Fatalf("record = %+v, %v", got, err)
	}
	// A clock that went back gives the new, earlier first time.
	got, err = s.MarkLimitFailure(ctx, "vision", true, base.Add(-time.Minute))
	if err != nil || got != (LimitStop{Workload: "vision", FirstSeenAt: base.Add(-time.Minute), DefiniteAt: base}) {
		t.Fatalf("record = %+v, %v", got, err)
	}
}
