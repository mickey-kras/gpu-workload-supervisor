package store

import (
	"context"
	"fmt"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

const schemaV1 = `
CREATE TABLE control_state (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    owner TEXT NOT NULL CHECK (owner IN ('supervisor', 'user')),
    desired_workload TEXT NOT NULL CHECK (desired_workload IN ('text', 'media', 'idle')),
    active_workload TEXT NOT NULL CHECK (active_workload IN ('text', 'media', 'idle', 'unknown')),
    phase TEXT NOT NULL CHECK (phase IN ('stable', 'draining', 'unloading', 'loading', 'verifying', 'reconciling')),
    health TEXT NOT NULL CHECK (health IN ('healthy', 'degraded', 'error')),
    admission TEXT NOT NULL CHECK (admission IN ('open', 'closed')),
    lease_incarnation TEXT NOT NULL,
    lease_epoch INTEGER NOT NULL CHECK (lease_epoch > 0),
    version INTEGER NOT NULL CHECK (version > 0),
    updated_at TEXT NOT NULL
);
CREATE TABLE transitions (
    transition_id TEXT PRIMARY KEY,
    lease_incarnation TEXT NOT NULL,
    lease_epoch INTEGER NOT NULL CHECK (lease_epoch > 0),
    source_state BLOB NOT NULL,
    target_state BLOB NOT NULL,
    previous_state BLOB NOT NULL,
    initiator TEXT NOT NULL,
    job_id TEXT,
    phase TEXT NOT NULL,
    deadline TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('in_progress', 'committed', 'failed')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE transition_events (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    transition_id TEXT NOT NULL REFERENCES transitions(transition_id),
    phase TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('intent', 'observation')),
    action TEXT NOT NULL,
    outcome TEXT,
    created_at TEXT NOT NULL
);
CREATE TABLE registered_work (
    request_id TEXT PRIMARY KEY,
    job_id TEXT,
    lease_incarnation TEXT NOT NULL,
    lease_epoch INTEGER NOT NULL CHECK (lease_epoch > 0),
    registered_at TEXT NOT NULL,
    completed_at TEXT
);
CREATE INDEX idx_registered_work_active
ON registered_work(completed_at) WHERE completed_at IS NULL;
`

const schemaV2 = `
CREATE TABLE transition_work (
    transition_id TEXT NOT NULL REFERENCES transitions(transition_id),
    request_id TEXT NOT NULL REFERENCES registered_work(request_id),
    PRIMARY KEY (transition_id, request_id)
);
`

const schemaV3 = `
ALTER TABLE registered_work
ADD COLUMN workload TEXT CHECK (workload IN ('text', 'media'));
`

const schemaV4 = `
ALTER TABLE registered_work
ADD COLUMN completion_outcome TEXT CHECK (completion_outcome IN ('completed', 'abandoned'));
`

var migrations = []string{schemaV1, schemaV2, schemaV3, schemaV4}

func (s *Store) initialize(ctx context.Context) error {
	for _, pragma := range []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = FULL",
		"PRAGMA busy_timeout = 5000",
	} {
		if _, err := s.db.ExecContext(ctx, pragma); err != nil {
			return fmt.Errorf("configure sqlite: %w", err)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("create migration table: %w", err)
	}
	var current int
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&current); err != nil {
		return err
	}
	if current > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than supported version %d", current, len(migrations))
	}
	for index := current; index < len(migrations); index++ {
		version := index + 1
		if _, err := tx.ExecContext(ctx, migrations[index]); err != nil {
			return fmt.Errorf("apply schema v%d: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at)
			VALUES (?, ?)`, version, formatTime(s.now())); err != nil {
			return err
		}
	}
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM control_state").Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		incarnation, err := s.uuid()
		if err != nil {
			return err
		}
		state := control.InitialState(incarnation, s.now())
		if _, err := tx.ExecContext(ctx, `INSERT INTO control_state
			(singleton, owner, desired_workload, active_workload, phase, health, admission,
			 lease_incarnation, lease_epoch, version, updated_at)
			VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			state.Owner, state.DesiredWorkload, state.ActiveWorkload, state.Phase,
			state.Health, state.Admission, state.LeaseFence.Incarnation,
			state.LeaseFence.Epoch, state.Version, formatTime(state.UpdatedAt)); err != nil {
			return err
		}
	}
	if count > 1 {
		return fmt.Errorf("control_state contains %d rows", count)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	var result string
	if err := s.db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("sqlite quick_check: %s", result)
	}
	state, err := readState(ctx, s.db)
	if err != nil {
		return err
	}
	return state.Validate()
}
