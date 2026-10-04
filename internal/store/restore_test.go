package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestRestoredBackupCannotReviveRetiredFence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "original.db")
	live, err := open(ctx, path, fixedClock(), fixedUUID("11111111-1111-4111-8111-111111111111"))
	if err != nil {
		t.Fatal(err)
	}
	state, err := live.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state.DesiredWorkload = control.WorkloadText
	state.ActiveWorkload = control.WorkloadText
	state.Phase = control.PhaseStable
	state.Admission = control.AdmissionOpen
	state, err = live.UpdateState(ctx, state.Version, state)
	if err != nil {
		t.Fatal(err)
	}
	oldFence := state.LeaseFence
	if err := live.AdmitWork(ctx, "before-restore", "", control.WorkloadText, oldFence); err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(t.TempDir(), "backup.db")
	if _, err := live.db.ExecContext(ctx, "VACUUM INTO ?", backupPath); err != nil {
		t.Fatal(err)
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	live, err = open(ctx, path, fixedClock(), fixedUUID("22222222-2222-4222-8222-222222222222"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := live.RotateFenceAndCloseAdmission(ctx, state.Version); err != nil {
		t.Fatal(err)
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}

	restoredPath := filepath.Join(t.TempDir(), "restored.db")
	if err := os.WriteFile(restoredPath, backup, 0o600); err != nil {
		t.Fatal(err)
	}
	restored, err := open(ctx, restoredPath, fixedClock(), fixedUUID("33333333-3333-4333-8333-333333333333"))
	if err != nil {
		t.Fatal(err)
	}
	unsafeState, err := restored.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if unsafeState.LeaseFence != oldFence || unsafeState.Admission != control.AdmissionOpen {
		t.Fatalf("backup did not reproduce old admitted fence: %#v", unsafeState)
	}
	safeState, err := restored.RotateIncarnation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if safeState.LeaseFence.Incarnation == oldFence.Incarnation ||
		safeState.LeaseFence.Epoch != 1 ||
		safeState.Admission != control.AdmissionClosed ||
		safeState.Phase != control.PhaseReconciling ||
		safeState.Health != control.HealthHealthy ||
		safeState.ActiveWorkload != control.WorkloadUnknown ||
		safeState.Owner != control.OwnerSupervisor {
		t.Fatalf("restored state is unsafe: %#v", safeState)
	}
	if err := restored.AdmitWork(ctx, "old-fence", "", control.WorkloadText, oldFence); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("old fence admission = %v, want stale fence", err)
	}
	if err := restored.AdmitWork(ctx, "new-fence", "", control.WorkloadText, safeState.LeaseFence); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatalf("new fence admission before reconciliation = %v, want closed", err)
	}
	var outcome string
	if err := restored.db.QueryRowContext(ctx, "SELECT completion_outcome FROM registered_work WHERE request_id = 'before-restore'").Scan(&outcome); err != nil {
		t.Fatal(err)
	}
	if outcome != string(WorkAbandoned) {
		t.Fatalf("restored work outcome = %q", outcome)
	}
	var previous, next string
	var abandoned, invalidated int
	if err := restored.db.QueryRowContext(ctx, `SELECT previous_incarnation, new_incarnation,
		abandoned_work, invalidated_transitions FROM state_restorations`).
		Scan(&previous, &next, &abandoned, &invalidated); err != nil {
		t.Fatal(err)
	}
	if previous != oldFence.Incarnation || next != safeState.LeaseFence.Incarnation || abandoned != 1 || invalidated != 0 {
		t.Fatalf("restore audit: previous=%q next=%q work=%d transitions=%d", previous, next, abandoned, invalidated)
	}
	if err := restored.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := open(ctx, restoredPath, fixedClock(), fixedUUID("44444444-4444-4444-8444-444444444444"))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	stateAfterRestart, err := reopened.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stateAfterRestart.LeaseFence != safeState.LeaseFence {
		t.Fatalf("ordinary restart changed fence: %#v", stateAfterRestart)
	}
}

func TestRestoreInvalidatesInProgressTransitionAndAbandonsWork(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state.DesiredWorkload = control.WorkloadText
	state.ActiveWorkload = control.WorkloadText
	state.Phase = control.PhaseStable
	state.Admission = control.AdmissionOpen
	state, err = s.UpdateState(ctx, state.Version, state)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AdmitWork(ctx, "active", "", control.WorkloadText, state.LeaseFence); err != nil {
		t.Fatal(err)
	}
	tr := Transition{ID: "interrupted", Source: state, Target: state, Previous: state,
		Initiator: "test", Deadline: time.Date(2026, 9, 29, 12, 5, 0, 0, time.UTC)}
	if _, err := s.StartTransition(ctx, state.Version, tr); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RotateIncarnation(ctx); err != nil {
		t.Fatal(err)
	}
	running, err := s.InProgressTransition(ctx)
	if err != nil || running != "" {
		t.Fatalf("running transition after restore = %q, %v", running, err)
	}
	var pending int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM registered_work WHERE completed_at IS NULL").Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("restored work still active: %d", pending)
	}
	events, err := s.TransitionEvents(ctx, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != "observation" || events[0].Action != "state restore" || events[0].Outcome != "invalidated" {
		t.Fatalf("restore transition event: %#v", events)
	}
	var invalidated, abandoned int
	if err := s.db.QueryRowContext(ctx, "SELECT invalidated_transitions, abandoned_work FROM state_restorations").
		Scan(&invalidated, &abandoned); err != nil {
		t.Fatal(err)
	}
	if invalidated != 1 || abandoned != 1 {
		t.Fatalf("restore counts: transitions=%d work=%d", invalidated, abandoned)
	}
}

func TestRestoreFailureRollsBackFenceAndWork(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state.DesiredWorkload = control.WorkloadText
	state.ActiveWorkload = control.WorkloadText
	state.Phase = control.PhaseStable
	state.Admission = control.AdmissionOpen
	before, err := s.UpdateState(ctx, state.Version, state)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AdmitWork(ctx, "active", "", control.WorkloadText, before.LeaseFence); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "DROP TABLE state_restorations"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RotateIncarnation(ctx); err == nil {
		t.Fatal("restore accepted an audit write failure")
	}
	after, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("failed restore changed state: %#v, want %#v", after, before)
	}
	var pending int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM registered_work WHERE completed_at IS NULL").Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("failed restore abandoned work: %d pending", pending)
	}
}

func TestRestoreRejectsReusedIncarnation(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	before, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.uuid = fixedUUID(before.LeaseFence.Incarnation)
	if _, err := s.RotateIncarnation(ctx); err == nil {
		t.Fatal("restore reused the old incarnation")
	}
	after, err := s.State(ctx)
	if err != nil || after != before {
		t.Fatalf("failed restore changed state: %#v, %v", after, err)
	}
}

func TestOpenRestoredRejectsUninitializedDatabaseWithoutMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "placeholder.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if opened, err := OpenRestored(ctx, path); err == nil {
		opened.Close()
		t.Fatal("empty placeholder was accepted as a restored backup")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Fatalf("empty placeholder was initialized: %d bytes", info.Size())
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE unrelated (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if opened, err := OpenRestored(ctx, path); err == nil {
		opened.Close()
		t.Fatal("uninitialized SQLite database was accepted as a restored backup")
	}
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var migrations int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE name = 'schema_migrations'").Scan(&migrations); err != nil {
		t.Fatal(err)
	}
	if migrations != 0 {
		t.Fatal("restore preflight initialized the database")
	}
}

func TestOpenRestoredMigratesV8WithoutLosingResolutionAudit(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "restored-v8.db")
	s, err := open(ctx, path, fixedClock(), fixedUUID("11111111-1111-4111-8111-111111111111"))
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO work_resolutions
		(lease_incarnation, lease_epoch, state_version, reason, abandoned_work, created_at)
		VALUES (?, ?, ?, 'incident-123', 1, ?)`, before.LeaseFence.Incarnation,
		before.LeaseFence.Epoch, before.Version, formatTime(fixedClock()())); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `DROP INDEX idx_transitions_in_progress_order`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `DROP INDEX idx_transition_events_transition_sequence; DROP TABLE workload_catalog; DROP TABLE workload_catalog_history; DELETE FROM schema_migrations WHERE version >= 9`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := OpenRestored(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	after, err := restored.State(ctx)
	if err != nil || after != before {
		t.Fatalf("migration changed restored state: %#v, %v", after, err)
	}
	var reason string
	if err := restored.db.QueryRowContext(ctx, `SELECT reason FROM work_resolutions`).Scan(&reason); err != nil || reason != "incident-123" {
		t.Fatalf("resolution audit after migration = %q, %v", reason, err)
	}
	var indexCount int
	if err := restored.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'index' AND name = 'idx_transitions_in_progress_order'`).Scan(&indexCount); err != nil || indexCount != 1 {
		t.Fatalf("transition index after migration = %d, %v", indexCount, err)
	}
}
