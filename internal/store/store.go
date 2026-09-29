package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	_ "modernc.org/sqlite"
)

var (
	ErrStaleFence       = errors.New("stale lease fence")
	ErrVersionConflict  = errors.New("control state version conflict")
	ErrAdmissionClosed  = errors.New("admission is closed")
	ErrWorkloadMismatch = errors.New("workload does not match active allocation")
)

type Clock func() time.Time

type Store struct {
	db   *sql.DB
	now  Clock
	uuid func() (string, error)
}

type Transition struct {
	ID        string
	Fence     control.Fence
	Source    control.State
	Target    control.State
	Previous  control.State
	Initiator string
	JobID     string
	Phase     control.Phase
	Deadline  time.Time
}

type TransitionEvent struct {
	Sequence     uint64
	TransitionID string
	Phase        control.Phase
	Kind         string
	Action       string
	Outcome      string
	CreatedAt    time.Time
}

func Open(ctx context.Context, path string) (*Store, error) {
	return open(ctx, path, time.Now, newUUID)
}

func OpenRestored(ctx context.Context, path string) (*Store, error) {
	return openWithMode(ctx, path, time.Now, newUUID, true)
}

func open(ctx context.Context, path string, now Clock, uuid func() (string, error)) (*Store, error) {
	return openWithMode(ctx, path, now, uuid, false)
}

func openWithMode(ctx context.Context, path string, now Clock, uuid func() (string, error), restored bool) (*Store, error) {
	if err := preparePath(path); err != nil {
		return nil, err
	}
	if restored {
		info, err := os.Lstat(path)
		if err != nil {
			return nil, fmt.Errorf("restored state database must already exist: %w", err)
		}
		if info.Size() == 0 {
			return nil, errors.New("restored state database is empty")
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect sqlite: %w", err)
	}
	if restored {
		if err := validateRestoredDatabase(ctx, db); err != nil {
			db.Close()
			return nil, err
		}
	}
	if err := os.Chmod(path, 0o600); err != nil {
		db.Close()
		return nil, fmt.Errorf("secure sqlite file: %w", err)
	}
	s := &Store{db: db, now: now, uuid: uuid}
	if err := s.initialize(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func validateRestoredDatabase(ctx context.Context, db *sql.DB) error {
	var result string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result); err != nil {
		return fmt.Errorf("check restored sqlite: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("sqlite quick_check: %s", result)
	}
	var migrations int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations").Scan(&migrations); err != nil {
		return fmt.Errorf("restored state database is not initialized: %w", err)
	}
	if migrations == 0 {
		return errors.New("restored state database is not initialized")
	}
	if _, err := readState(ctx, db); err != nil {
		return fmt.Errorf("restored control state is invalid: %w", err)
	}
	return nil
}

func (s *Store) Close() error                                     { return s.db.Close() }
func (s *Store) State(ctx context.Context) (control.State, error) { return readState(ctx, s.db) }

func (s *Store) UpdateState(ctx context.Context, expected uint64, next control.State) (control.State, error) {
	if err := next.Validate(); err != nil {
		return control.State{}, fmt.Errorf("validate state: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return control.State{}, err
	}
	defer tx.Rollback()
	current, err := readState(ctx, tx)
	if err != nil {
		return control.State{}, err
	}
	if current.Version != expected {
		return control.State{}, ErrVersionConflict
	}
	if next.LeaseFence != current.LeaseFence {
		return control.State{}, ErrStaleFence
	}
	next.Version = current.Version + 1
	next.UpdatedAt = s.now().UTC()
	if err := writeState(ctx, tx, next); err != nil {
		return control.State{}, err
	}
	if err := tx.Commit(); err != nil {
		return control.State{}, err
	}
	return next, nil
}

func (s *Store) RotateFenceAndCloseAdmission(ctx context.Context, expected uint64) (control.State, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return control.State{}, err
	}
	defer tx.Rollback()
	state, err := readState(ctx, tx)
	if err != nil {
		return control.State{}, err
	}
	if state.Version != expected {
		return control.State{}, ErrVersionConflict
	}
	state.LeaseFence.Epoch++
	state.Admission = control.AdmissionClosed
	state.Version++
	state.UpdatedAt = s.now().UTC()
	if err := writeState(ctx, tx, state); err != nil {
		return control.State{}, err
	}
	if err := tx.Commit(); err != nil {
		return control.State{}, err
	}
	return state, nil
}

func (s *Store) RotateIncarnation(ctx context.Context) (control.State, error) {
	incarnation, err := s.uuid()
	if err != nil {
		return control.State{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return control.State{}, err
	}
	defer tx.Rollback()
	state, err := readState(ctx, tx)
	if err != nil {
		return control.State{}, err
	}
	if incarnation == state.LeaseFence.Incarnation {
		return control.State{}, errors.New("new lease incarnation matches restored incarnation")
	}
	previous := state.LeaseFence
	now := formatTime(s.now())
	if _, err := tx.ExecContext(ctx, `INSERT INTO transition_events
		(transition_id, phase, kind, action, outcome, created_at)
		SELECT transition_id, ?, 'observation', 'state restore', 'invalidated', ?
		FROM transitions WHERE status = 'in_progress'`,
		control.PhaseReconciling, now); err != nil {
		return control.State{}, fmt.Errorf("record interrupted transitions: %w", err)
	}
	transitions, err := tx.ExecContext(ctx, `UPDATE transitions
		SET phase = ?, status = 'failed', updated_at = ?
		WHERE status = 'in_progress'`, control.PhaseReconciling, now)
	if err != nil {
		return control.State{}, fmt.Errorf("invalidate interrupted transitions: %w", err)
	}
	invalidated, err := transitions.RowsAffected()
	if err != nil {
		return control.State{}, err
	}
	work, err := tx.ExecContext(ctx, `UPDATE registered_work
		SET completed_at = ?, completion_outcome = 'abandoned'
		WHERE completed_at IS NULL`, now)
	if err != nil {
		return control.State{}, fmt.Errorf("abandon restored work: %w", err)
	}
	abandoned, err := work.RowsAffected()
	if err != nil {
		return control.State{}, err
	}
	state.Owner = control.OwnerSupervisor
	state.DesiredWorkload = control.WorkloadIdle
	state.LeaseFence = control.Fence{Incarnation: incarnation, Epoch: 1}
	state.Admission = control.AdmissionClosed
	state.Phase = control.PhaseReconciling
	state.Health = control.HealthHealthy
	state.ActiveWorkload = control.WorkloadUnknown
	state.Version++
	state.UpdatedAt = s.now().UTC()
	if err := state.Validate(); err != nil {
		return control.State{}, err
	}
	if err := writeState(ctx, tx, state); err != nil {
		return control.State{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO state_restorations
		(previous_incarnation, previous_epoch, new_incarnation, new_epoch,
		 abandoned_work, invalidated_transitions, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		previous.Incarnation, previous.Epoch, state.LeaseFence.Incarnation,
		state.LeaseFence.Epoch, abandoned, invalidated, now); err != nil {
		return control.State{}, fmt.Errorf("record state restoration: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return control.State{}, err
	}
	return state, nil
}

func (s *Store) RegisterWork(ctx context.Context, requestID, jobID string, workload control.Workload, fence control.Fence) error {
	tx, err := s.beginAdmittedWork(ctx, requestID, workload, fence)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO registered_work
		(request_id, job_id, workload, lease_incarnation, lease_epoch, registered_at)
		VALUES (?, ?, ?, ?, ?, ?)`, requestID, nullable(jobID), workload, fence.Incarnation, fence.Epoch, formatTime(s.now()))
	if err != nil {
		return fmt.Errorf("register work: %w", err)
	}
	return tx.Commit()
}

func (s *Store) BeginTransition(ctx context.Context, tr Transition) error {
	if tr.ID == "" {
		return errors.New("transition id is empty")
	}
	if err := tr.Fence.Validate(); err != nil {
		return fmt.Errorf("invalid transition fence: %w", err)
	}
	source, err := json.Marshal(tr.Source)
	if err != nil {
		return err
	}
	target, err := json.Marshal(tr.Target)
	if err != nil {
		return err
	}
	previous, err := json.Marshal(tr.Previous)
	if err != nil {
		return err
	}
	now := formatTime(s.now())
	_, err = s.db.ExecContext(ctx, `INSERT INTO transitions
		(transition_id, lease_incarnation, lease_epoch, source_state, target_state,
		 previous_state, initiator, job_id, phase, deadline, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'in_progress', ?, ?)`,
		tr.ID, tr.Fence.Incarnation, tr.Fence.Epoch, source, target, previous,
		tr.Initiator, nullable(tr.JobID), tr.Phase, formatTime(tr.Deadline), now, now)
	if err != nil {
		return fmt.Errorf("begin transition: %w", err)
	}
	return nil
}

func (s *Store) AppendTransitionEvent(ctx context.Context, event TransitionEvent) error {
	if event.TransitionID == "" || event.Action == "" {
		return errors.New("transition id and action are required")
	}
	if event.Kind != "intent" && event.Kind != "observation" {
		return errors.New("event kind must be intent or observation")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO transition_events
		(transition_id, phase, kind, action, outcome, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		event.TransitionID, event.Phase, event.Kind, event.Action, nullable(event.Outcome), formatTime(s.now()))
	return err
}

func (s *Store) TransitionEvents(ctx context.Context, transitionID string) ([]TransitionEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sequence, transition_id, phase, kind, action,
		COALESCE(outcome, ''), created_at FROM transition_events
		WHERE transition_id = ? ORDER BY sequence`, transitionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []TransitionEvent
	for rows.Next() {
		var event TransitionEvent
		var created string
		if err := rows.Scan(&event.Sequence, &event.TransitionID, &event.Phase, &event.Kind,
			&event.Action, &event.Outcome, &created); err != nil {
			return nil, err
		}
		event.CreatedAt, err = parseTime(created)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

type querier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readState(ctx context.Context, q querier) (control.State, error) {
	var state control.State
	var updated string
	err := q.QueryRowContext(ctx, `SELECT owner, desired_workload, active_workload, phase,
		health, admission, lease_incarnation, lease_epoch, version, updated_at
		FROM control_state WHERE singleton = 1`).Scan(
		&state.Owner, &state.DesiredWorkload, &state.ActiveWorkload, &state.Phase,
		&state.Health, &state.Admission, &state.LeaseFence.Incarnation,
		&state.LeaseFence.Epoch, &state.Version, &updated)
	if err != nil {
		return state, err
	}
	state.UpdatedAt, err = parseTime(updated)
	if err != nil {
		return state, err
	}
	return state, state.Validate()
}

func writeState(ctx context.Context, tx *sql.Tx, state control.State) error {
	result, err := tx.ExecContext(ctx, `UPDATE control_state SET owner = ?, desired_workload = ?,
		active_workload = ?, phase = ?, health = ?, admission = ?, lease_incarnation = ?,
		lease_epoch = ?, version = ?, updated_at = ? WHERE singleton = 1`,
		state.Owner, state.DesiredWorkload, state.ActiveWorkload, state.Phase, state.Health,
		state.Admission, state.LeaseFence.Incarnation, state.LeaseFence.Epoch,
		state.Version, formatTime(state.UpdatedAt))
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func preparePath(path string) error {
	if path == "" {
		return errors.New("database path is empty")
	}
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return err
	}
	if parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() {
		return errors.New("database parent must be a directory, not a symlink")
	}
	if parentInfo.Mode().Perm()&0o022 != 0 {
		return errors.New("database parent must not be group or world writable")
	}
	stat, ok := parentInfo.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return errors.New("database parent must be owned by the current user")
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("database path must not be a symlink")
		}
		if !info.Mode().IsRegular() {
			return errors.New("database path must be a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func newUUID() (string, error) {
	value, err := uuid.NewRandom()
	return value.String(), err
}

func formatTime(t time.Time) string         { return t.UTC().Format(time.RFC3339Nano) }
func parseTime(v string) (time.Time, error) { return time.Parse(time.RFC3339Nano, v) }
func nullable(v string) any {
	if v == "" {
		return nil
	}
	return v
}
