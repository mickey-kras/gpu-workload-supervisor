package setup

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	_ "modernc.org/sqlite"
)

// Inspect opens the existing database read-only. In particular, activation must
// never call store.Open (which migrates) before quiescence and backup succeed.
func Inspect(ctx context.Context, path string) error {
	db, err := readOnlyDB(path)
	if err != nil {
		return err
	}
	defer db.Close()
	var active, desired, phase, admission, health string
	if err := db.QueryRowContext(ctx, `SELECT active_workload,desired_workload,phase,admission,health FROM control_state WHERE singleton=1`).Scan(&active, &desired, &phase, &admission, &health); err != nil {
		return err
	}
	if active != "idle" || desired != "idle" || phase != "stable" || admission != "closed" || health == "error" {
		return errors.New("activation requires explicitly quiesced healthy Idle with closed admission")
	}
	var pending, running int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM registered_work WHERE completed_at IS NULL`).Scan(&pending); err != nil {
		return err
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM transitions WHERE status='in_progress'`).Scan(&running); err != nil {
		return err
	}
	if pending != 0 || running != 0 {
		return errors.New("activation refuses unfinished work or transitions")
	}
	return integrity(ctx, db)
}
func readOnlyDB(path string) (*sql.DB, error) {
	file, err := deployment.OpenPrivate(path)
	if err != nil {
		return nil, err
	}
	file.Close()
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}
	return sql.Open("sqlite", u.String())
}
func integrity(ctx context.Context, db *sql.DB) error {
	var result string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("database integrity: %s", result)
	}
	rows, err := db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return errors.New("database foreign key check failed")
	}
	return rows.Err()
}

// Backup takes a consistent SQLite snapshot including WAL contents, then verifies
// it. An existing snapshot is never overwritten, allowing interrupted activation
// to resume without destroying the original compatible state.
func Backup(ctx context.Context, path, destination string) error {
	if _, err := os.Lstat(destination); err == nil {
		db, err := readOnlyDB(destination)
		if err != nil {
			return err
		}
		defer db.Close()
		return integrity(ctx, db)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	db, err := readOnlyDB(path)
	if err != nil {
		return err
	}
	defer db.Close()
	temp, err := os.CreateTemp(filepath.Dir(destination), ".backup-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	temp.Close()
	defer os.Remove(name)
	// VACUUM INTO requires a nonexistent or empty output file. The private inode
	// is precreated to avoid exposing state under a permissive process umask.
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", name); err != nil {
		return err
	}
	snapshot, err := readOnlyDB(name)
	if err != nil {
		return err
	}
	err = integrity(ctx, snapshot)
	snapshot.Close()
	if err != nil {
		return err
	}
	file, err := os.Open(name)
	if err != nil {
		return err
	}
	err = file.Sync()
	file.Close()
	if err != nil {
		return err
	}
	if err := os.Rename(name, destination); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(destination))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// The mirror file is a rollback artifact and never a live configuration source.
func ReadCatalog(ctx context.Context, path string) (control.CatalogSnapshot, error) {
	var snapshot control.CatalogSnapshot
	db, err := readOnlyDB(path)
	if err != nil {
		return snapshot, err
	}
	defer db.Close()
	var exists int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='workload_catalog'").Scan(&exists); err != nil {
		return snapshot, err
	}
	if exists == 0 {
		return snapshot, nil
	}
	var data []byte
	err = db.QueryRowContext(ctx, "SELECT revision,catalog FROM workload_catalog WHERE singleton=1").Scan(&snapshot.Revision, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return snapshot, nil
	}
	if err != nil {
		return snapshot, err
	}
	snapshot.Catalog, err = control.DecodeCatalog(bytes.NewReader(data))
	return snapshot, err
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
