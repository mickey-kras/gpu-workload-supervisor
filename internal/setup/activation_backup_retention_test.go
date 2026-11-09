package setup

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestPruneActivationBackupsKeepsNewestTuples(t *testing.T) {
	root := t.TempDir()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	names := []string{"activation-a", "activation-b", "activation-c", "activation-d", "activation-e"}
	for i, name := range names {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Join(path, "owned-units"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "state.db"), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
		modified := base.Add(time.Duration(i) * time.Hour)
		if err := os.Chtimes(path, modified, modified); err != nil {
			t.Fatal(err)
		}
	}
	// Unrelated entries are never touched.
	if err := os.WriteFile(filepath.Join(root, "notes"), []byte("user"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := pruneActivationBackups(root, 3); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, entry := range entries {
		got = append(got, entry.Name())
	}
	if want := []string{"activation-c", "activation-d", "activation-e", "notes"}; !slices.Equal(got, want) {
		t.Fatalf("retained %v, want %v", got, want)
	}
}

func TestPruneActivationBackupsMissingRoot(t *testing.T) {
	if err := pruneActivationBackups(filepath.Join(t.TempDir(), "absent"), 3); err != nil {
		t.Fatal(err)
	}
}

func TestRepeatedActivationsBoundBackups(t *testing.T) {
	backend, home, r := fixture(t)
	ctx := t.Context()
	for range retainedActivationBackups + 3 {
		if err := backend.Apply(ctx, home, r); err != nil {
			t.Fatal(err)
		}
		snapshot, err := ReadCatalog(ctx, r.Profile.StatePath)
		if err != nil {
			t.Fatal(err)
		}
		r.ExpectedRevision = snapshot.Revision
	}
	backups, err := filepath.Glob(filepath.Join(home, ".config/gpu-workload-supervisor/backups/activation-*"))
	if err != nil || len(backups) != retainedActivationBackups {
		t.Fatalf("retained %d backups, want %d: %v", len(backups), retainedActivationBackups, err)
	}
}

func TestFailedActivationBackupLeavesNoPartialTuple(t *testing.T) {
	backend, home, r := fixture(t)
	ctx := t.Context()
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	snapshot, err := ReadCatalog(ctx, r.Profile.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	r.ExpectedRevision = snapshot.Revision
	root := filepath.Join(home, ".config/gpu-workload-supervisor")
	// copyActivation fails after the tuple directory and state.db exist.
	if err := os.Remove(filepath.Join(root, "activated-binaries/manifest.json")); err != nil {
		t.Fatal(err)
	}
	if err := backend.Apply(ctx, home, r); err == nil {
		t.Fatal("activation without a binary tuple accepted")
	}
	backups, err := filepath.Glob(filepath.Join(root, "backups/activation-*"))
	if err != nil || len(backups) != 0 {
		t.Fatalf("partial backup tuples left behind: %v %v", backups, err)
	}
}
