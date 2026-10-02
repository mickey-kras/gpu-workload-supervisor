package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func admitRetentionWork(t *testing.T, s *Store, ids ...string) control.State {
	t.Helper()
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
	for _, id := range ids {
		if err := s.AdmitWork(ctx, id, "", control.WorkloadText, state.LeaseFence); err != nil {
			t.Fatal(err)
		}
	}
	return state
}

func finishRetentionWork(t *testing.T, s *Store, state control.State, id string, completedAt time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := s.FinishWorkFenced(ctx, id, control.WorkloadText, state.LeaseFence, WorkCompleted); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE registered_work SET completed_at = ? WHERE request_id = ?`,
		formatTime(completedAt), id); err != nil {
		t.Fatal(err)
	}
}

func retentionWorkExists(t *testing.T, s *Store, id string) bool {
	t.Helper()
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM registered_work WHERE request_id = ?`, id).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count == 1
}

func TestPruneCompletedWorkPreservesActiveAndRunningSnapshot(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	state := admitRetentionWork(t, s, "old", "terminal", "running", "active", "recent")
	old := fixedClock()().Add(-48 * time.Hour)
	for _, id := range []string{"old", "terminal", "running"} {
		finishRetentionWork(t, s, state, id, old)
	}
	finishRetentionWork(t, s, state, "recent", fixedClock()().Add(-time.Hour))
	for _, id := range []string{"done", "in-progress"} {
		tr := Transition{ID: id, Fence: state.LeaseFence, Source: state, Target: state,
			Previous: state, Initiator: "test", Phase: control.PhaseDraining,
			Deadline: fixedClock()().Add(time.Hour)}
		if err := s.BeginTransition(ctx, tr); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AppendTransitionEvent(ctx, TransitionEvent{TransitionID: "done",
		Phase: control.PhaseDraining, Kind: "observation", Action: "complete", Outcome: "ok"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE transitions SET status = 'committed' WHERE transition_id = 'done'`); err != nil {
		t.Fatal(err)
	}
	for _, link := range [][2]string{{"done", "terminal"}, {"in-progress", "running"}} {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO transition_work (transition_id, request_id) VALUES (?, ?)`,
			link[0], link[1]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.RotateFenceAndCloseAdmission(ctx, state.Version); err != nil {
		t.Fatal(err)
	}
	count, err := s.PruneCompletedWork(ctx, fixedClock()().Add(-24*time.Hour), 256)
	if err != nil || count != 2 {
		t.Fatalf("prune = %d, %v; want 2", count, err)
	}
	for _, id := range []string{"old", "terminal"} {
		if retentionWorkExists(t, s, id) {
			t.Fatalf("%s was not pruned", id)
		}
	}
	for _, id := range []string{"running", "active", "recent"} {
		if !retentionWorkExists(t, s, id) {
			t.Fatalf("%s was pruned", id)
		}
	}
	var terminalLinks, runningLinks int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM transition_work WHERE transition_id = 'done'`).Scan(&terminalLinks); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM transition_work WHERE transition_id = 'in-progress'`).Scan(&runningLinks); err != nil {
		t.Fatal(err)
	}
	if terminalLinks != 0 || runningLinks != 1 {
		t.Fatalf("snapshot links: terminal=%d running=%d", terminalLinks, runningLinks)
	}
	events, err := s.TransitionEvents(ctx, "done")
	if err != nil || len(events) != 1 || events[0].Action != "complete" {
		t.Fatalf("transition audit events = %#v, %v", events, err)
	}
	var transitions int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM transitions`).Scan(&transitions); err != nil {
		t.Fatal(err)
	}
	if transitions != 2 {
		t.Fatalf("transition records = %d", transitions)
	}
	if pending, err := s.PendingTransitionWork(ctx, "in-progress"); err != nil || pending != 0 {
		t.Fatalf("completed running snapshot pending = %d, %v", pending, err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE transitions SET status = 'failed' WHERE transition_id = 'in-progress'`); err != nil {
		t.Fatal(err)
	}
	count, err = s.PruneCompletedWork(ctx, fixedClock()().Add(-24*time.Hour), 256)
	if err != nil || count != 1 || retentionWorkExists(t, s, "running") {
		t.Fatalf("prune after terminal transition = %d, %v", count, err)
	}
}

func TestPruneKeepsCurrentFenceRequestIDsUntilRotation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	state := admitRetentionWork(t, s, "same-id")
	finishRetentionWork(t, s, state, "same-id", fixedClock()().Add(-48*time.Hour))
	count, err := s.PruneCompletedWork(ctx, fixedClock()().Add(-24*time.Hour), 256)
	if err != nil || count != 0 || !retentionWorkExists(t, s, "same-id") {
		t.Fatalf("current fence prune = %d, %v", count, err)
	}
	if err := s.AdmitWork(ctx, "same-id", "", control.WorkloadText, state.LeaseFence); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("current fence duplicate accepted: %v", err)
	}
	rotated, err := s.RotateFenceAndCloseAdmission(ctx, state.Version)
	if err != nil {
		t.Fatal(err)
	}
	count, err = s.PruneCompletedWork(ctx, fixedClock()().Add(-24*time.Hour), 256)
	if err != nil || count != 1 || retentionWorkExists(t, s, "same-id") {
		t.Fatalf("retired fence prune = %d, %v", count, err)
	}
	rotated.Admission = control.AdmissionOpen
	rotated, err = s.UpdateState(ctx, rotated.Version, rotated)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AdmitWork(ctx, "same-id", "", control.WorkloadText, rotated.LeaseFence); err != nil {
		t.Fatalf("new fence admission = %v", err)
	}
	if err := s.FinishWorkFenced(ctx, "same-id", control.WorkloadText, state.LeaseFence, WorkCompleted); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("old completion affected new work: %v", err)
	}
	var active int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM registered_work
		WHERE request_id = 'same-id' AND completed_at IS NULL`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("new work active count = %d", active)
	}
}

func TestPruneCompletedWorkIsBoundedAndHonorsCutoff(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	state := admitRetentionWork(t, s, "first", "second", "third", "later-in-cutoff-second")
	for _, id := range []string{"first", "second", "third"} {
		finishRetentionWork(t, s, state, id, fixedClock()().Add(-2*time.Second))
	}
	finishRetentionWork(t, s, state, "later-in-cutoff-second", fixedClock()().Add(600*time.Millisecond))
	if _, err := s.RotateFenceAndCloseAdmission(ctx, state.Version); err != nil {
		t.Fatal(err)
	}
	if count, err := s.PruneCompletedWork(ctx, fixedClock()(), 2); err != nil || count != 2 {
		t.Fatalf("first batch = %d, %v", count, err)
	}
	if count, err := s.PruneCompletedWork(ctx, fixedClock()(), 2); err != nil || count != 1 {
		t.Fatalf("second batch = %d, %v", count, err)
	}
	if !retentionWorkExists(t, s, "later-in-cutoff-second") {
		t.Fatal("completion after cutoff in same second was pruned")
	}
	if count, err := s.PruneCompletedWork(ctx, fixedClock()().Add(2*time.Second), 2); err != nil || count != 1 {
		t.Fatalf("third batch = %d, %v", count, err)
	}
	for _, limit := range []int{0, -1, 1025} {
		if _, err := s.PruneCompletedWork(ctx, fixedClock()(), limit); err == nil {
			t.Fatalf("accepted prune limit %d", limit)
		}
	}
	if _, err := s.PruneCompletedWork(ctx, time.Time{}, 2); err == nil {
		t.Fatal("accepted zero cutoff")
	}
	var remaining int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM registered_work WHERE completed_at IS NOT NULL`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("completed rows remain: %d", remaining)
	}
}

func TestTokenedWorkPrunesUnderCurrentFenceWithoutLateCompletionCollision(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	state := admitRetentionWork(t, s)
	firstToken, err := s.AdmitWorkToken(ctx, "same-id", "", control.WorkloadText, state.LeaseFence)
	if err != nil || firstToken == "" {
		t.Fatalf("first admission token = %q, %v", firstToken, err)
	}
	if err := s.FinishWorkToken(ctx, "same-id", control.WorkloadText, state.LeaseFence, "", WorkCompleted); !errors.Is(err, ErrRegistrationTokenMismatch) {
		t.Fatalf("tokenless completion = %v", err)
	}
	if err := s.FinishWorkToken(ctx, "same-id", control.WorkloadText, state.LeaseFence, "incorrect", WorkCompleted); !errors.Is(err, ErrRegistrationTokenMismatch) {
		t.Fatalf("wrong token completion = %v", err)
	}
	if err := s.FinishWorkToken(ctx, "same-id", control.WorkloadText, state.LeaseFence, firstToken, WorkCompleted); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE registered_work SET completed_at = ? WHERE request_id = ?`,
		formatTime(fixedClock()().Add(-48*time.Hour)), "same-id"); err != nil {
		t.Fatal(err)
	}
	if count, err := s.PruneCompletedWork(ctx, fixedClock()().Add(-24*time.Hour), 256); err != nil || count != 1 {
		t.Fatalf("current fence tokened prune = %d, %v", count, err)
	}
	secondToken, err := s.AdmitWorkToken(ctx, "same-id", "", control.WorkloadText, state.LeaseFence)
	if err != nil || secondToken == firstToken {
		t.Fatalf("second admission token = %q, %v", secondToken, err)
	}
	if err := s.FinishWorkToken(ctx, "same-id", control.WorkloadText, state.LeaseFence, firstToken, WorkCompleted); !errors.Is(err, ErrRegistrationTokenMismatch) {
		t.Fatalf("old token completion = %v", err)
	}
	var active int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM registered_work
		WHERE request_id = 'same-id' AND completed_at IS NULL`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("new work active count = %d", active)
	}
	if err := s.FinishWorkToken(ctx, "same-id", control.WorkloadText, state.LeaseFence, secondToken, WorkCompleted); err != nil {
		t.Fatalf("second completion = %v", err)
	}
}

func TestPruneCompletedWorkWithNoEligibleRows(t *testing.T) {
	s := testStore(t)
	count, err := s.PruneCompletedWork(context.Background(), fixedClock()().Add(-24*time.Hour), 256)
	if err != nil || count != 0 {
		t.Fatalf("empty prune = %d, %v", count, err)
	}
}

func TestPruneCompletedWorkReportsSelectionFailure(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, "DROP TABLE registered_work"); err != nil {
		t.Fatal(err)
	}
	_, err := s.PruneCompletedWork(ctx, fixedClock()().Add(-24*time.Hour), 256)
	if err == nil || !strings.Contains(err.Error(), "select completed work to prune") {
		t.Fatalf("selection failure = %v", err)
	}
}

func TestPruneCompletedWorkRollsBackOnDeleteFailure(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	state := admitRetentionWork(t, s)
	token, err := s.AdmitWorkToken(ctx, "delete-failure", "", control.WorkloadText, state.LeaseFence)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishWorkToken(ctx, "delete-failure", control.WorkloadText, state.LeaseFence, token, WorkCompleted); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE registered_work SET completed_at = ? WHERE request_id = ?",
		formatTime(fixedClock()().Add(-48*time.Hour)), "delete-failure"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER deny_work_delete BEFORE DELETE ON registered_work
		BEGIN SELECT RAISE(ABORT, 'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	_, err = s.PruneCompletedWork(ctx, fixedClock()().Add(-24*time.Hour), 256)
	if err == nil || !strings.Contains(err.Error(), "prune completed work") {
		t.Fatalf("delete failure = %v", err)
	}
	if !retentionWorkExists(t, s, "delete-failure") {
		t.Fatal("work was removed despite failed prune transaction")
	}
}
