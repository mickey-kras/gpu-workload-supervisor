package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	_ "modernc.org/sqlite"
)

var (
	ErrStaleFence      = errors.New("stale lease fence")
	ErrVersionConflict = errors.New("control state version conflict")
	ErrAdmissionClosed = errors.New("admission is closed")
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

func open(ctx context.Context, path string, now Clock, uuid func() (string, error)) (*Store, error) {
	if err := preparePath(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db, now: now, uuid: uuid}
	if err := s.initialize(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
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
	state.LeaseFence = control.Fence{Incarnation: incarnation, Epoch: 1}
	state.Admission = control.AdmissionClosed
	state.Phase = control.PhaseReconciling
	state.ActiveWorkload = control.WorkloadUnknown
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

func (s *Store) RegisterWork(ctx context.Context, requestID, jobID string, fence control.Fence) error {
	if requestID == "" {
		return errors.New("request id is empty")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	state, err := readState(ctx, tx)
	if err != nil {
		return err
	}
	if state.LeaseFence != fence {
		return ErrStaleFence
	}
	if state.Admission != control.AdmissionOpen {
		return ErrAdmissionClosed
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO registered_work
		(request_id, job_id, lease_incarnation, lease_epoch, registered_at)
		VALUES (?, ?, ?, ?, ?)`, requestID, nullable(jobID), fence.Incarnation, fence.Epoch, formatTime(s.now()))
	if err != nil {
		return fmt.Errorf("register work: %w", err)
	}
	return tx.Commit()
}

func (s *Store) CompleteWork(ctx context.Context, requestID string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE registered_work SET completed_at = ?
		WHERE request_id = ? AND completed_at IS NULL`, formatTime(s.now()), requestID)
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
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
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
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	buf := make([]byte, 36)
	hex.Encode(buf[0:8], raw[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], raw[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], raw[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], raw[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:36], raw[10:16])
	return string(buf), nil
}

func formatTime(t time.Time) string         { return t.UTC().Format(time.RFC3339Nano) }
func parseTime(v string) (time.Time, error) { return time.Parse(time.RFC3339Nano, v) }
func nullable(v string) any {
	if v == "" {
		return nil
	}
	return v
}
