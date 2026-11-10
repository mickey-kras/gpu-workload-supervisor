package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// preMigrationSnapshotsKept bounds snapshot growth. Each snapshot is a full
// copy of the database. Three covers the last few upgrades, so an operator can
// still roll back after a quick second upgrade. It matches the three activation
// tuples that setup keeps.
const preMigrationSnapshotsKept = 3

const snapshotTimeLayout = "20060102T150405Z"

// preMigrationSnapshotPath names a snapshot next to the database. The name
// shows the schema version the snapshot holds and the UTC time it was taken.
func preMigrationSnapshotPath(path string, from int, at time.Time) string {
	return fmt.Sprintf("%s.pre-v%d-%s.bak", path, from, at.UTC().Format(snapshotTimeLayout))
}

func snapshotNamePattern(path string) *regexp.Regexp {
	return regexp.MustCompile(`^` + regexp.QuoteMeta(filepath.Base(path)) + `\.pre-v[0-9]+-([0-9]{8}T[0-9]{6}Z)\.bak$`)
}

// snapshotBeforeMigration runs while the migration transaction holds the
// write lock and has not written yet. A separate read connection therefore
// copies exactly the committed schema-v<from> state, and no other process can
// migrate it between the copy and the upgrade.
func (s *Store) snapshotBeforeMigration(ctx context.Context, from int) error {
	path, err := filepath.Abs(s.statePath)
	if err != nil {
		return fmt.Errorf("resolve sqlite path: %w", err)
	}
	destination := preMigrationSnapshotPath(path, from, s.now())
	if err := writeSnapshot(ctx, path, destination, from); err != nil {
		return err
	}
	if err := pruneSnapshots(path, destination, preMigrationSnapshotsKept); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

// writeSnapshot copies source with VACUUM INTO, which reads a consistent view
// that includes WAL contents. The copy is verified and synced under a private
// temporary name, then linked to destination. Linking never replaces a file.
func writeSnapshot(ctx context.Context, source, destination string, version int) error {
	directory := filepath.Dir(destination)
	// CreateTemp makes a mode-0600 inode, so state is never exposed under a
	// permissive umask. VACUUM INTO accepts an existing empty output file.
	temp, err := os.CreateTemp(directory, "."+filepath.Base(source)+".snapshot-*")
	if err != nil {
		return fmt.Errorf("create snapshot file: %w", err)
	}
	name := temp.Name()
	err = temp.Close()
	if err == nil {
		err = buildSnapshot(ctx, source, name, version)
	}
	if err == nil {
		if linkErr := os.Link(name, destination); linkErr != nil {
			err = fmt.Errorf("publish snapshot: %w", linkErr)
		}
	}
	if removeErr := os.Remove(name); removeErr != nil {
		err = errors.Join(err, fmt.Errorf("remove snapshot temporary file: %w", removeErr))
	}
	if err != nil {
		return err
	}
	return syncDirectory(directory)
}

func buildSnapshot(ctx context.Context, source, name string, version int) error {
	db, err := openReadOnly(source)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, "VACUUM INTO ?", name)
	if closeErr := db.Close(); closeErr != nil {
		err = errors.Join(err, closeErr)
	}
	if err != nil {
		return fmt.Errorf("copy database: %w", err)
	}
	if err := verifySnapshot(ctx, name, version); err != nil {
		return err
	}
	file, err := os.Open(name)
	if err != nil {
		return fmt.Errorf("open snapshot: %w", err)
	}
	err = file.Sync()
	if closeErr := file.Close(); closeErr != nil {
		err = errors.Join(err, closeErr)
	}
	if err != nil {
		return fmt.Errorf("sync snapshot: %w", err)
	}
	return nil
}

// verifySnapshot requires a clean integrity check and the expected schema
// version, so a snapshot can never hold already-migrated state.
func verifySnapshot(ctx context.Context, name string, version int) error {
	db, err := openReadOnly(name)
	if err != nil {
		return err
	}
	defer db.Close()
	var result string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil {
		return fmt.Errorf("check snapshot integrity: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("snapshot integrity: %s", result)
	}
	var current int
	if err := db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&current); err != nil {
		return fmt.Errorf("read snapshot schema version: %w", err)
	}
	if current != version {
		return fmt.Errorf("snapshot schema version %d, want %d", current, version)
	}
	return nil
}

func openReadOnly(path string) (*sql.DB, error) {
	options := url.Values{"mode": {"ro"}, "_pragma": {"busy_timeout(5000)"}}
	dsn := url.URL{Scheme: "file", Path: path, RawQuery: options.Encode()}
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, fmt.Errorf("open sqlite read-only: %w", err)
	}
	return db, nil
}

// pruneSnapshots keeps the newest snapshots of the database at path. It always
// keeps the snapshot just written, even if the clock moved backward.
func pruneSnapshots(path, keep string, count int) error {
	directory := filepath.Dir(path)
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("list snapshots: %w", err)
	}
	pattern := snapshotNamePattern(path)
	type snapshot struct{ name, taken string }
	var older []snapshot
	for _, entry := range entries {
		match := pattern.FindStringSubmatch(entry.Name())
		if match == nil || !entry.Type().IsRegular() || entry.Name() == filepath.Base(keep) {
			continue
		}
		older = append(older, snapshot{name: entry.Name(), taken: match[1]})
	}
	sort.Slice(older, func(i, j int) bool {
		if older[i].taken != older[j].taken {
			return older[i].taken > older[j].taken
		}
		return older[i].name > older[j].name
	})
	for index := count - 1; index < len(older); index++ {
		if err := os.Remove(filepath.Join(directory, older[index].name)); err != nil {
			return fmt.Errorf("remove old snapshot: %w", err)
		}
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open directory: %w", err)
	}
	err = directory.Sync()
	if closeErr := directory.Close(); closeErr != nil {
		err = errors.Join(err, closeErr)
	}
	if err != nil {
		return fmt.Errorf("sync directory: %w", err)
	}
	return nil
}
