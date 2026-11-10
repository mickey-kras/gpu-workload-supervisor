package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func clockAt(at time.Time) Clock { return func() time.Time { return at } }

// downgradeToV18 opens a current database and removes the v19 migration, so
// the next open must migrate it again.
func downgradeToV18(t *testing.T, path string) {
	t.Helper()
	ctx := context.Background()
	s, err := open(ctx, path, fixedClock(), fixedUUID("11111111-1111-4111-8111-111111111111"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO model_checks(model,payload) VALUES('model-a','{}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `DROP TABLE limit_stops; DELETE FROM schema_migrations WHERE version >= 19`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func snapshotNames(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".pre-v") || strings.Contains(entry.Name(), ".snapshot-") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names
}

func schemaVersion(t *testing.T, path string) int {
	t.Helper()
	db, err := openReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}

func TestMigrationTakesVerifiedPrivateSnapshot(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	downgradeToV18(t, path)
	if names := snapshotNames(t, path); len(names) != 0 {
		t.Fatalf("fresh database took snapshots: %v", names)
	}
	at := time.Date(2026, 11, 10, 8, 30, 5, 0, time.FixedZone("x", 3600))
	s, err := open(ctx, path, clockAt(at), fixedUUID("22222222-2222-4222-8222-222222222222"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	want := "state.db.pre-v18-20261110T073005Z.bak"
	if names := snapshotNames(t, path); len(names) != 1 || names[0] != want {
		t.Fatalf("snapshots = %v, want [%s]", names, want)
	}
	snapshot := filepath.Join(filepath.Dir(path), want)
	info, err := os.Stat(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("snapshot mode %v", info.Mode().Perm())
	}
	if version := schemaVersion(t, snapshot); version != 18 {
		t.Fatalf("snapshot version %d", version)
	}
	if err := verifySnapshot(ctx, snapshot, 18); err != nil {
		t.Fatal(err)
	}
	db, err := openReadOnly(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var model string
	if err := db.QueryRow(`SELECT model FROM model_checks`).Scan(&model); err != nil || model != "model-a" {
		t.Fatalf("snapshot rows: %q %v", model, err)
	}
	if version := schemaVersion(t, path); version != len(migrations) {
		t.Fatalf("live version %d", version)
	}
	// A restored snapshot opens and migrates like any older database.
	restored := filepath.Join(t.TempDir(), "state.db")
	data, err := os.ReadFile(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(restored, data, 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := OpenRestored(ctx, restored)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if names := snapshotNames(t, restored); len(names) != 1 || !strings.HasPrefix(names[0], "state.db.pre-v18-") {
		t.Fatalf("restored snapshots = %v", names)
	}
}

func TestNoSnapshotWhenSchemaIsCurrent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	for attempt := 0; attempt < 2; attempt++ {
		s, err := open(ctx, path, fixedClock(), fixedUUID("11111111-1111-4111-8111-111111111111"))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if names := snapshotNames(t, path); len(names) != 0 {
		t.Fatalf("snapshots = %v", names)
	}
}

func TestSnapshotFailureAbortsMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	downgradeToV18(t, path)
	// An existing file at the snapshot name makes publication fail. Linking
	// never replaces it.
	blocked := preMigrationSnapshotPath(path, 18, fixedClock()())
	if err := os.WriteFile(blocked, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := open(ctx, path, fixedClock(), fixedUUID("22222222-2222-4222-8222-222222222222")); err == nil || !strings.Contains(err.Error(), "snapshot before schema v19 migration") {
		t.Fatalf("open error = %v", err)
	}
	if version := schemaVersion(t, path); version != 18 {
		t.Fatalf("migrated to %d despite snapshot failure", version)
	}
	if data, err := os.ReadFile(blocked); err != nil || string(data) != "keep" {
		t.Fatalf("existing file replaced: %q %v", data, err)
	}
	if names := snapshotNames(t, path); len(names) != 1 {
		t.Fatalf("temporary files left: %v", names)
	}
}

func TestSnapshotRetentionKeepsNewest(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	foreign := filepath.Join(filepath.Dir(path), "other.db.pre-v1-20200101T000000Z.bak")
	if err := os.WriteFile(foreign, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var want []string
	for day := 0; day < preMigrationSnapshotsKept+2; day++ {
		downgradeToV18(t, path)
		at := base.AddDate(0, 0, day)
		s, err := open(ctx, path, clockAt(at), fixedUUID("22222222-2222-4222-8222-222222222222"))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		want = append(want, filepath.Base(preMigrationSnapshotPath(path, 18, at)))
	}
	want = want[len(want)-preMigrationSnapshotsKept:]
	if names := snapshotNames(t, path); strings.Join(names, ",") != strings.Join(append([]string{"other.db.pre-v1-20200101T000000Z.bak"}, want...), ",") {
		t.Fatalf("snapshots = %v, want newest %v", names, want)
	}
	// A clock that moved backward never prunes the snapshot just taken.
	downgradeToV18(t, path)
	s, err := open(ctx, path, clockAt(base.AddDate(-1, 0, 0)), fixedUUID("33333333-3333-4333-8333-333333333333"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(preMigrationSnapshotPath(path, 18, base.AddDate(-1, 0, 0))); err != nil {
		t.Fatalf("new snapshot pruned: %v", err)
	}
	if names := snapshotNames(t, path); len(names) != preMigrationSnapshotsKept+1 {
		t.Fatalf("snapshots = %v", names)
	}
}

func TestVerifySnapshotRejectsCorruptAndWrongVersion(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	corrupt := filepath.Join(directory, "corrupt.bak")
	if err := os.WriteFile(corrupt, []byte(strings.Repeat("not a database ", 512)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifySnapshot(ctx, corrupt, 18); err == nil {
		t.Fatal("corrupt snapshot verified")
	}
	path := filepath.Join(directory, "state.db")
	s, err := open(ctx, path, fixedClock(), fixedUUID("11111111-1111-4111-8111-111111111111"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := verifySnapshot(ctx, path, 18); err == nil || !strings.Contains(err.Error(), "want 18") {
		t.Fatalf("wrong version error = %v", err)
	}
	if err := writeSnapshot(ctx, path, filepath.Join(directory, "missing", "x.bak"), 18); err == nil {
		t.Fatal("snapshot into missing directory succeeded")
	}
	if err := pruneSnapshots(filepath.Join(directory, "missing", "state.db"), "", 1); err == nil {
		t.Fatal("prune of missing directory succeeded")
	}
	if err := syncDirectory(filepath.Join(directory, "missing")); err == nil {
		t.Fatal("sync of missing directory succeeded")
	}
}

func TestBuildSnapshotRejectsUnusableOutputAndSchema(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	path := filepath.Join(directory, "state.db")
	s, err := open(ctx, path, fixedClock(), fixedUUID("11111111-1111-4111-8111-111111111111"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// VACUUM INTO cannot write into a missing directory.
	unusable := filepath.Join(directory, "missing", "x.bak")
	if err := buildSnapshot(ctx, path, unusable, len(migrations)); err == nil || !strings.Contains(err.Error(), "copy database") {
		t.Fatalf("non-empty output error = %v", err)
	}
	// A copy of the wrong version fails verification before it is published.
	empty := filepath.Join(directory, "empty.bak")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := buildSnapshot(ctx, path, empty, 1); err == nil || !strings.Contains(err.Error(), "want 1") {
		t.Fatalf("wrong version error = %v", err)
	}
	// A database without migration metadata is not a state snapshot.
	plain := filepath.Join(directory, "plain.db")
	db, err := sql.Open("sqlite", plain)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE t(x)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := verifySnapshot(ctx, plain, 1); err == nil || !strings.Contains(err.Error(), "schema version") {
		t.Fatalf("missing metadata error = %v", err)
	}
}
