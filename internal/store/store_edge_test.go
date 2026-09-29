package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestRegisterWorkRequiresOpenAdmissionAndCompletesOnce(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterWork(ctx, "", "job", control.WorkloadText, state.LeaseFence); err == nil {
		t.Fatal("empty request id accepted")
	}
	if err := s.RegisterWork(ctx, "request", "job", control.WorkloadText, state.LeaseFence); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatalf("closed admission error = %v", err)
	}
	state.Phase = control.PhaseStable
	state.Admission = control.AdmissionOpen
	state.DesiredWorkload = control.WorkloadText
	state.ActiveWorkload = control.WorkloadText
	opened, err := s.UpdateState(ctx, state.Version, state)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterWork(ctx, "request", "", control.WorkloadText, opened.LeaseFence); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterWork(ctx, "request", "job", control.WorkloadText, opened.LeaseFence); err == nil {
		t.Fatal("duplicate request id accepted")
	}
	if err := s.CompleteWork(ctx, "request"); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteWork(ctx, "request"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("duplicate completion error = %v", err)
	}
	if err := s.CompleteWork(ctx, "missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown completion error = %v", err)
	}
	rotated, err := s.RotateFenceAndCloseAdmission(ctx, opened.Version)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterWork(ctx, "late", "job", control.WorkloadText, opened.LeaseFence); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("old fence error = %v", err)
	}
	if err := s.RegisterWork(ctx, "late", "job", control.WorkloadText, rotated.LeaseFence); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatalf("rotated admission error = %v", err)
	}
}

func TestStateMutationRejectsConflictAndFenceReplacement(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	invalid := state
	invalid.Phase = control.PhaseLoading
	invalid.Admission = control.AdmissionOpen
	if _, err := s.UpdateState(ctx, state.Version, invalid); err == nil {
		t.Fatal("unsafe admission accepted")
	}
	if _, err := s.UpdateState(ctx, state.Version+1, state); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("version conflict = %v", err)
	}
	if _, err := s.RotateFenceAndCloseAdmission(ctx, state.Version+1); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("fence rotation conflict = %v", err)
	}
	changedFence := state
	changedFence.LeaseFence.Epoch++
	if _, err := s.UpdateState(ctx, state.Version, changedFence); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("fence replacement = %v", err)
	}
	updated, err := s.UpdateState(ctx, state.Version, state)
	if err != nil || updated.Version != state.Version+1 {
		t.Fatalf("valid update = %#v, %v", updated, err)
	}
	s.uuid = func() (string, error) { return "", errors.New("uuid unavailable") }
	if _, err := s.RotateIncarnation(ctx); err == nil {
		t.Fatal("incarnation rotation accepted a failed UUID source")
	}
}

func TestTransitionJournalRejectsInvalidOrOrphanEvents(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tr := Transition{ID: "transition", Fence: state.LeaseFence, Source: state, Target: state,
		Previous: state, Initiator: "test", Phase: control.PhaseDraining,
		Deadline: time.Date(2026, 9, 29, 12, 5, 0, 0, time.UTC)}
	bad := tr
	bad.ID = ""
	if err := s.BeginTransition(ctx, bad); err == nil {
		t.Fatal("empty transition id accepted")
	}
	bad = tr
	bad.Fence.Epoch = 0
	if err := s.BeginTransition(ctx, bad); err == nil {
		t.Fatal("invalid transition fence accepted")
	}
	if err := s.BeginTransition(ctx, tr); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginTransition(ctx, tr); err == nil {
		t.Fatal("duplicate transition accepted")
	}
	for _, event := range []TransitionEvent{
		{TransitionID: tr.ID, Kind: "intent"},
		{TransitionID: tr.ID, Action: "stop", Kind: "other"},
		{TransitionID: "missing", Action: "stop", Kind: "intent"},
	} {
		if err := s.AppendTransitionEvent(ctx, event); err == nil {
			t.Fatalf("invalid event accepted: %#v", event)
		}
	}
	if events, err := s.TransitionEvents(ctx, tr.ID); err != nil || len(events) != 0 {
		t.Fatalf("invalid events persisted: %#v, %v", events, err)
	}
}

func TestOpenRejectsUnsafePathAndCorruptTimestamp(t *testing.T) {
	ctx := context.Background()
	if _, err := Open(ctx, ""); err == nil {
		t.Fatal("empty database path accepted")
	}
	dir := t.TempDir()
	if _, err := Open(ctx, dir); err == nil {
		t.Fatal("directory accepted as database file")
	}
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, link); err == nil {
		t.Fatal("symlink accepted as database file")
	}
	s := testStore(t)
	if _, err := s.db.ExecContext(ctx, "UPDATE control_state SET updated_at = 'bad' WHERE singleton = 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.State(ctx); err == nil {
		t.Fatal("invalid database timestamp accepted")
	}
}

func TestNewUUIDHasVersionAndVariant(t *testing.T) {
	uuid, err := newUUID()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-4[a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$`).MatchString(uuid) {
		t.Fatalf("invalid UUID: %q", uuid)
	}
}

func TestCanceledContextCannotMutateStateOrJournal(t *testing.T) {
	s := testStore(t)
	state, err := s.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, operation := range map[string]func() error{
		"update":      func() error { _, err := s.UpdateState(ctx, state.Version, state); return err },
		"fence":       func() error { _, err := s.RotateFenceAndCloseAdmission(ctx, state.Version); return err },
		"incarnation": func() error { _, err := s.RotateIncarnation(ctx); return err },
		"register":    func() error { return s.RegisterWork(ctx, "request", "job", control.WorkloadText, state.LeaseFence) },
		"complete":    func() error { return s.CompleteWork(ctx, "request") },
		"transition":  func() error { return s.BeginTransition(ctx, Transition{ID: "transition", Fence: state.LeaseFence}) },
		"events":      func() error { _, err := s.TransitionEvents(ctx, "transition"); return err },
		"state":       func() error { _, err := s.State(ctx); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := operation(); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled operation error = %v", err)
			}
		})
	}
	after, err := s.State(context.Background())
	if err != nil || after != state {
		t.Fatalf("canceled operations changed state: %#v, %v", after, err)
	}
}

func TestCorruptPersistedStateAndEventTimestampFailClosed(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, "UPDATE control_state SET admission = 'open' WHERE singleton = 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.State(ctx); err == nil {
		t.Fatal("unsafe persisted admission was accepted")
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE control_state SET admission = 'closed' WHERE singleton = 1"); err != nil {
		t.Fatal(err)
	}
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tr := Transition{ID: "transition", Fence: state.LeaseFence, Source: state, Target: state,
		Previous: state, Deadline: time.Date(2026, 9, 29, 12, 5, 0, 0, time.UTC)}
	if err := s.BeginTransition(ctx, tr); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendTransitionEvent(ctx, TransitionEvent{TransitionID: tr.ID, Kind: "intent", Action: "stop"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE transition_events SET created_at = 'bad'"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionEvents(ctx, tr.ID); err == nil {
		t.Fatal("corrupt event timestamp was accepted")
	}
}

func TestStateWriteFailurePreservesFenceAndVersion(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	before, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER reject_control_update
		BEFORE UPDATE ON control_state BEGIN SELECT RAISE(ABORT, 'write blocked'); END`); err != nil {
		t.Fatal(err)
	}
	for name, operation := range map[string]func() error{
		"update":      func() error { _, err := s.UpdateState(ctx, before.Version, before); return err },
		"fence":       func() error { _, err := s.RotateFenceAndCloseAdmission(ctx, before.Version); return err },
		"incarnation": func() error { _, err := s.RotateIncarnation(ctx); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := operation(); err == nil {
				t.Fatal("failed write was accepted")
			}
			after, err := s.State(ctx)
			if err != nil || after != before {
				t.Fatalf("failed write changed state: %#v, %v", after, err)
			}
		})
	}
}

func TestOpenRejectsInvalidDatabaseAndUnwritableParent(t *testing.T) {
	dir := t.TempDir()
	invalid := filepath.Join(dir, "invalid.db")
	if err := os.WriteFile(invalid, []byte("not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), invalid); err == nil {
		t.Fatal("invalid database accepted")
	}
	parentFile := filepath.Join(dir, "parent")
	if err := os.WriteFile(parentFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), filepath.Join(parentFile, "state.db")); err == nil {
		t.Fatal("database created under a regular file")
	}
}

func TestInitializationRejectsUUIDFailureAndDuplicateControlRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "uuid.db")
	if _, err := open(ctx, path, fixedClock(), func() (string, error) {
		return "", errors.New("uuid unavailable")
	}); err == nil {
		t.Fatal("new store accepted a failed UUID source")
	}
	duplicate := filepath.Join(t.TempDir(), "duplicate.db")
	db, err := sql.Open("sqlite", duplicate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE control_state (singleton INTEGER);
		INSERT INTO control_state (singleton) VALUES (1), (2)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := open(ctx, duplicate, fixedClock(), fixedUUID("incarnation")); err == nil {
		t.Fatal("duplicate persisted control rows accepted")
	}
}

func TestIgnoredStateWriteDoesNotClaimSuccess(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	before, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER ignore_control_update
		BEFORE UPDATE ON control_state BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateState(ctx, before.Version, before); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("ignored write error = %v", err)
	}
	after, err := s.State(ctx)
	if err != nil || after != before {
		t.Fatalf("ignored write changed state: %#v, %v", after, err)
	}
}
