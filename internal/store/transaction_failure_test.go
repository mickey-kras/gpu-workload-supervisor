package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestTransitionRollsBackWhenWorkSnapshotFails(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	initial, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "DROP TABLE transition_work"); err != nil {
		t.Fatal(err)
	}
	_, err = s.StartTransition(ctx, initial.Version, Transition{
		ID: "snapshot-failed", Target: initial, Previous: initial,
		Deadline: time.Now().Add(time.Minute),
	})
	if err == nil {
		t.Fatal("transition committed without work snapshot")
	}
	after, err := s.State(ctx)
	if err != nil || after != initial {
		t.Fatalf("partial state update = %#v, error = %v", after, err)
	}
	if id, err := s.InProgressTransition(ctx); err != nil || id != "" {
		t.Fatalf("partial transition = %q, error = %v", id, err)
	}
}

func TestRecoveryRollsBackWhenAuditCannotBeWritten(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	initial, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	running, err := s.StartTransition(ctx, initial.Version, Transition{
		ID: "audit-failed", Target: initial, Previous: initial,
		Deadline: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "DROP TABLE transition_events"); err != nil {
		t.Fatal(err)
	}
	final := running
	final.ActiveWorkload = control.WorkloadUnknown
	final.Phase = control.PhaseReconciling
	final.Admission = control.AdmissionClosed
	if _, err := s.Recover(ctx, running.Version, final, "failed"); err == nil {
		t.Fatal("recovery committed without audit event")
	}
	after, err := s.State(ctx)
	if err != nil || after != running {
		t.Fatalf("recovery changed state without audit: %#v, error = %v", after, err)
	}
	if id, err := s.InProgressTransition(ctx); err != nil || id != "audit-failed" {
		t.Fatalf("transition lost after failed recovery: %q, error = %v", id, err)
	}
}

func TestStorageFailureRejectsAllStateMutations(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for name, operation := range map[string]func() error{
		"state update":         func() error { _, err := s.UpdateState(ctx, state.Version, state); return err },
		"fence rotation":       func() error { _, err := s.RotateFenceAndCloseAdmission(ctx, state.Version); return err },
		"incarnation rotation": func() error { _, err := s.RotateIncarnation(ctx); return err },
		"work registration":    func() error { return s.RegisterWork(ctx, "request", "job", control.WorkloadText, state.LeaseFence) },
		"work completion": func() error {
			return s.FinishWorkFenced(ctx, "request", control.WorkloadText, state.LeaseFence, WorkCompleted)
		},
		"transition start": func() error {
			_, err := s.StartTransition(ctx, state.Version, Transition{ID: "closed", Target: state})
			return err
		},
		"transition phase": func() error {
			_, err := s.SetTransitionPhase(ctx, "closed", state.Version, control.PhaseLoading)
			return err
		},
		"transition finish": func() error {
			_, err := s.FinishTransition(ctx, "closed", "failed", state.Version, state)
			return err
		},
		"recovery": func() error { _, err := s.Recover(ctx, state.Version, state, "closed"); return err },
	} {
		if err := operation(); err == nil {
			t.Fatalf("%s accepted a closed store", name)
		}
	}
	if err := s.initialize(ctx); err == nil {
		t.Fatal("closed database accepted initialization")
	}
}

func TestOpenRejectsUnsafeParentDirectory(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	file := filepath.Join(dir, "parent-file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, filepath.Join(file, "state.db")); err == nil {
		t.Fatal("regular file accepted as database parent")
	}
	link := filepath.Join(dir, "parent-link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, filepath.Join(link, "state.db")); err == nil {
		t.Fatal("symlink accepted as database parent")
	}
	worldWritable := filepath.Join(dir, "writable")
	if err := os.Mkdir(worldWritable, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(worldWritable, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, filepath.Join(worldWritable, "state.db")); err == nil {
		t.Fatal("world-writable database parent accepted")
	}
}

func TestOpenRejectsFutureOrBrokenMigrationHistory(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*Store) error
	}{
		{"future schema", func(s *Store) error {
			_, err := s.db.Exec("INSERT INTO schema_migrations(version, applied_at) VALUES (999, '2026-09-29T00:00:00Z')")
			return err
		}},
		{"invalid history", func(s *Store) error {
			_, err := s.db.Exec("DROP TABLE schema_migrations")
			if err != nil {
				return err
			}
			_, err = s.db.Exec("CREATE TABLE schema_migrations (unexpected INTEGER)")
			return err
		}},
		{"corrupt state", func(s *Store) error {
			_, err := s.db.Exec("UPDATE control_state SET updated_at = 'invalid' WHERE singleton = 1")
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			s, err := Open(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			if err := test.change(s); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if reopened, err := Open(context.Background(), path); err == nil {
				reopened.Close()
				t.Fatal("incompatible persisted database accepted")
			}
		})
	}
}

func TestMissingStateTableRejectsEveryTransactionalMutation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "DROP TABLE control_state"); err != nil {
		t.Fatal(err)
	}
	for name, operation := range map[string]func() error{
		"update":      func() error { _, err := s.UpdateState(ctx, state.Version, state); return err },
		"fence":       func() error { _, err := s.RotateFenceAndCloseAdmission(ctx, state.Version); return err },
		"incarnation": func() error { _, err := s.RotateIncarnation(ctx); return err },
		"admission":   func() error { return s.RegisterWork(ctx, "request", "job", control.WorkloadText, state.LeaseFence) },
		"transition": func() error {
			_, err := s.StartTransition(ctx, state.Version, Transition{ID: "broken", Target: state})
			return err
		},
		"phase": func() error {
			_, err := s.SetTransitionPhase(ctx, "broken", state.Version, control.PhaseLoading)
			return err
		},
		"finish": func() error {
			_, err := s.FinishTransition(ctx, "broken", "failed", state.Version, state)
			return err
		},
		"recovery": func() error { _, err := s.Recover(ctx, state.Version, state, "broken"); return err },
	} {
		if err := operation(); err == nil {
			t.Fatalf("%s accepted missing state table", name)
		}
	}
}

func TestMissingTransitionJournalCannotAdvanceOrRecover(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	initial, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	running, err := s.StartTransition(ctx, initial.Version, Transition{
		ID: "running", Target: initial, Previous: initial,
		Deadline: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "DROP TABLE transitions"); err != nil {
		t.Fatal(err)
	}
	for name, operation := range map[string]func() error{
		"phase": func() error {
			_, err := s.SetTransitionPhase(ctx, "running", running.Version, control.PhaseLoading)
			return err
		},
		"finish": func() error {
			_, err := s.FinishTransition(ctx, "running", "failed", running.Version, running)
			return err
		},
		"recovery": func() error {
			_, err := s.Recover(ctx, running.Version, running, "journal missing")
			return err
		},
	} {
		if err := operation(); err == nil {
			t.Fatalf("%s accepted missing transition journal", name)
		}
	}
	after, err := s.State(ctx)
	if err != nil || after != running {
		t.Fatalf("journal failure changed control state: %#v, %v", after, err)
	}
}

func TestTransitionInsertRejectsMissingJournalWithoutFencingAdmission(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	initial, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "DROP TABLE transitions"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartTransition(ctx, initial.Version, Transition{
		ID: "unrecorded", Target: initial, Previous: initial,
		Deadline: time.Now().Add(time.Minute),
	}); err == nil {
		t.Fatal("transition began without a journal")
	}
	after, err := s.State(ctx)
	if err != nil || after != initial {
		t.Fatalf("unrecorded transition changed state: %#v, %v", after, err)
	}
}
