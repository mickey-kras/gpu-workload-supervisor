package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestTransitionSnapshotsOutstandingWorkAndCommits(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state.Phase = control.PhaseStable
	state.DesiredWorkload = control.WorkloadText
	state.ActiveWorkload = control.WorkloadText
	state.Admission = control.AdmissionOpen
	state, err = s.UpdateState(ctx, state.Version, state)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterWork(ctx, "work", "job", control.WorkloadText, state.LeaseFence); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterWork(ctx, "other-work", "other-job", control.WorkloadText, state.LeaseFence); err != nil {
		t.Fatal(err)
	}
	target := state
	target.DesiredWorkload = control.WorkloadMedia
	tr := Transition{ID: "transition", Fence: state.LeaseFence, Source: state, Target: target,
		Previous: state, Initiator: "test", Deadline: fixedClock()().Add(time.Minute)}
	started, err := s.StartTransition(ctx, state.Version, tr)
	if err != nil {
		t.Fatal(err)
	}
	if started.Phase != control.PhaseDraining || started.Admission != control.AdmissionClosed ||
		started.LeaseFence.Epoch != state.LeaseFence.Epoch+1 || started.DesiredWorkload != control.WorkloadMedia {
		t.Fatalf("transition did not fence admission: %#v", started)
	}
	if id, err := s.InProgressTransition(ctx); err != nil || id != tr.ID {
		t.Fatalf("in-progress transition = %q, %v", id, err)
	}
	if count, err := s.PendingTransitionWork(ctx, tr.ID); err != nil || count != 1 {
		t.Fatalf("snapshot pending work = %d, %v", count, err)
	}
	if err := s.FinishWorkFenced(ctx, "work", control.WorkloadText, state.LeaseFence, WorkCompleted); err != nil {
		t.Fatal(err)
	}
	if pending, err := s.PendingTransitionWork(ctx, tr.ID); err != nil || pending != 1 {
		t.Fatalf("other unfinished work was not retained: %d, %v", pending, err)
	}
	if err := s.FinishWorkFenced(ctx, "other-work", control.WorkloadText, state.LeaseFence, WorkCompleted); err != nil {
		t.Fatal(err)
	}
	if count, err := s.PendingTransitionWork(ctx, tr.ID); err != nil || count != 0 {
		t.Fatalf("completed work still pending = %d, %v", count, err)
	}
	phase, err := s.SetTransitionPhase(ctx, tr.ID, started.Version, control.PhaseUnloading)
	if err != nil || phase.Phase != control.PhaseUnloading {
		t.Fatalf("phase update = %#v, %v", phase, err)
	}
	final := phase
	final.Phase = control.PhaseStable
	final.ActiveWorkload = control.WorkloadMedia
	committed, err := s.FinishTransition(ctx, tr.ID, "committed", phase.Version, final)
	if err != nil || committed.Version != phase.Version+1 {
		t.Fatalf("commit = %#v, %v", committed, err)
	}
	if id, err := s.InProgressTransition(ctx); err != nil || id != "" {
		t.Fatalf("committed transition remains active = %q, %v", id, err)
	}
	if _, err := s.SetTransitionPhase(ctx, tr.ID, committed.Version, control.PhaseLoading); !errors.Is(err, ErrTransitionNotRunning) {
		t.Fatalf("closed transition accepted phase update: %v", err)
	}
}

func TestRecoveryFailsInterruptedTransitionAndRotatesFence(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tr := Transition{ID: "interrupted", Target: state, Previous: state,
		Deadline: fixedClock()().Add(time.Minute)}
	started, err := s.StartTransition(ctx, state.Version, tr)
	if err != nil {
		t.Fatal(err)
	}
	final := started
	final.Phase = control.PhaseReconciling
	final.DesiredWorkload = control.WorkloadIdle
	final.ActiveWorkload = control.WorkloadUnknown
	recovered, err := s.Recover(ctx, started.Version, final, "interrupted")
	if err != nil {
		t.Fatal(err)
	}
	if recovered.LeaseFence.Epoch != started.LeaseFence.Epoch+1 || recovered.Admission != control.AdmissionClosed {
		t.Fatalf("recovery did not fence admission: %#v", recovered)
	}
	if id, err := s.InProgressTransition(ctx); err != nil || id != "" {
		t.Fatalf("interrupted transition still active = %q, %v", id, err)
	}
	events, err := s.TransitionEvents(ctx, tr.ID)
	if err != nil || len(events) != 1 || events[0].Action != "recovery" || events[0].Outcome != "interrupted" {
		t.Fatalf("recovery audit = %#v, %v", events, err)
	}
	again, err := s.Recover(ctx, recovered.Version, recovered, "no active transition")
	if err != nil || again.LeaseFence.Epoch != recovered.LeaseFence.Epoch+1 {
		t.Fatalf("recovery without transition = %#v, %v", again, err)
	}
}

func TestTransitionRejectsInvalidOrStaleChanges(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tr := Transition{ID: "transition", Target: state, Previous: state,
		Deadline: fixedClock()().Add(time.Minute)}
	if _, err := s.StartTransition(ctx, state.Version, Transition{}); err == nil {
		t.Fatal("empty transition id accepted")
	}
	if _, err := s.StartTransition(ctx, state.Version+1, tr); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale start = %v", err)
	}
	bad := tr
	bad.Target.DesiredWorkload = "unknown"
	if _, err := s.StartTransition(ctx, state.Version, bad); err == nil {
		t.Fatal("invalid target accepted")
	}
	started, err := s.StartTransition(ctx, state.Version, tr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartTransition(ctx, started.Version, tr); err == nil {
		t.Fatal("duplicate transition id accepted")
	}
	if _, err := s.SetTransitionPhase(ctx, tr.ID, state.Version, control.PhaseLoading); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale phase update = %v", err)
	}
	if _, err := s.SetTransitionPhase(ctx, "missing", started.Version, control.PhaseLoading); !errors.Is(err, ErrTransitionNotRunning) {
		t.Fatalf("unknown transition phase update = %v", err)
	}
	if _, err := s.SetTransitionPhase(ctx, tr.ID, started.Version, "unknown"); err == nil {
		t.Fatal("invalid phase accepted")
	}
	for _, status := range []string{"", "in_progress"} {
		if _, err := s.FinishTransition(ctx, tr.ID, status, started.Version, started); err == nil {
			t.Fatalf("invalid finish status %q accepted", status)
		}
	}
	if _, err := s.FinishTransition(ctx, tr.ID, "failed", state.Version, started); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale finish = %v", err)
	}
	badFinal := started
	badFinal.LeaseFence.Epoch++
	if _, err := s.FinishTransition(ctx, tr.ID, "failed", started.Version, badFinal); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("stale finish fence = %v", err)
	}
	if _, err := s.FinishTransition(ctx, "missing", "failed", started.Version, started); !errors.Is(err, ErrTransitionNotRunning) {
		t.Fatalf("unknown transition finish = %v", err)
	}
	if _, err := s.Recover(ctx, state.Version, started, "stale"); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale recovery = %v", err)
	}
}
