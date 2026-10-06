package setup

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

func TestReadOnlyInspectionBackupAndUnsafeStates(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "state.db")
	if err := Inspect(ctx, path); err == nil {
		t.Fatal("missing accepted")
	}
	s, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Inspect(ctx, path); err == nil {
		t.Fatal("unreconciled accepted")
	}
	state, _ := s.State(ctx)
	state.ActiveWorkload = control.WorkloadIdle
	state.Phase = control.PhaseStable
	if _, err := s.UpdateState(ctx, state.Version, state); err != nil {
		t.Fatal(err)
	}
	if err := Inspect(ctx, path); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, "snapshot.db")
	if err := Backup(ctx, path, dest); err != nil {
		t.Fatal(err)
	}
	if err := Backup(ctx, path, dest); err != nil {
		t.Fatal(err)
	}
	if err := Backup(ctx, path, filepath.Join(root, "missing", "snapshot")); err == nil {
		t.Fatal("missing directory")
	}
	s.Close()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("UPDATE control_state SET admission='open'"); err != nil {
		t.Fatal(err)
	}
	if err := Inspect(ctx, path); err == nil {
		t.Fatal("open admission")
	}
	if _, err := db.Exec("UPDATE control_state SET admission='closed'; DROP TABLE registered_work"); err != nil {
		t.Fatal(err)
	}
	if err := Inspect(ctx, path); err == nil {
		t.Fatal("missing work table")
	}
	if err := os.WriteFile(filepath.Join(root, "corrupt.db"), []byte("not SQLite"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Inspect(ctx, filepath.Join(root, "corrupt.db")); err == nil {
		t.Fatal("corrupt state")
	}
	if err := Backup(ctx, filepath.Join(root, "corrupt.db"), filepath.Join(root, "bad-snapshot.db")); err == nil {
		t.Fatal("corrupt backup")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := integrity(canceled, db); err == nil {
		t.Fatal("canceled inspection")
	}
	if err := integrity(ctx, db); err != nil {
		t.Fatal(err)
	}
}
func TestBinaryTupleCorruptionAndPreservation(t *testing.T) {
	backend, home, r := fixture(t)
	ctx := context.Background()
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, ".config/gpu-workload-supervisor")
	profile, _ := os.ReadFile(filepath.Join(root, "operator.json"))
	dest := t.TempDir()
	if err := copyActivation(context.Background(), root, dest, profile); err != nil {
		t.Fatal(err)
	}
	if err := copyActivation(context.Background(), root, dest, profile); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "operator.json"), []byte("edited"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := copyActivation(context.Background(), root, dest, profile); err == nil {
		t.Fatal("overwrote tuple")
	}
	if err := os.WriteFile(filepath.Join(root, "activated-binaries/gpu-mode"), []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := copyActivation(context.Background(), root, t.TempDir(), profile); err == nil {
		t.Fatal("accepted tampered binary")
	}
	if err := os.Remove(filepath.Join(root, "activated-binaries/manifest.json")); err != nil {
		t.Fatal(err)
	}
	if err := copyActivation(context.Background(), root, t.TempDir(), profile); err == nil {
		t.Fatal("missing manifest")
	}
	if err := os.Chmod(filepath.Join(backend.binaryDirectory, "gpu-mode"), 0777); err != nil {
		t.Fatal(err)
	}
	if err := backend.retainBinaries(root); err == nil {
		t.Fatal("untrusted package binary")
	}
	os.Remove(filepath.Join(backend.binaryDirectory, "gpu-mode"))
	if err := backend.retainBinaries(root); err == nil {
		t.Fatal("missing package binary")
	}
}
