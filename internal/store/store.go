package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
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
	userExecutionLock string
	db                *sql.DB
	now               Clock
	uuid              func() (string, error)
}

type Transition struct {
	ConfigurationRevision string
	ID                    string
	Fence                 control.Fence
	Source                control.State
	Target                control.State
	Previous              control.State
	Initiator             string
	JobID                 string
	Phase                 control.Phase
	Deadline              time.Time
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
	return open(ctx, path, time.Now, control.NewUUID)
}

func OpenRestored(ctx context.Context, path string) (*Store, error) {
	return openWithMode(ctx, path, time.Now, control.NewUUID, true)
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
	if err := secureStateFile(path); err != nil {
		return nil, err
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve sqlite path: %w", err)
	}
	// Every writable transaction must reserve the writer before reading state.
	// Otherwise an independent proxy retention commit can invalidate its WAL
	// snapshot, and the read-to-write upgrade fails with SQLITE_BUSY_SNAPSHOT.
	// Plain queries (and explicitly read-only transactions) remain readers.
	// database/sql may replace an interrupted connection. Apply connection-local
	// safety settings on every open, not only while initializing the schema.
	options := url.Values{"_txlock": {"immediate"}, "_pragma": {
		"busy_timeout(5000)", "foreign_keys(ON)", "journal_mode(WAL)", "synchronous(FULL)",
	}}
	dsn := url.URL{Scheme: "file", Path: absolutePath, RawQuery: options.Encode()}
	db, err := sql.Open("sqlite", dsn.String())
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
	s := &Store{db: db, now: now, uuid: uuid, userExecutionLock: path + ".user-execution.lock"}
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
	if _, err := readControlState(ctx, db); err != nil {
		return fmt.Errorf("restored control state is invalid: %w", err)
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) State(ctx context.Context) (control.State, error) { return readState(ctx, s.db) }

func (s *Store) UpdateState(ctx context.Context, expected uint64, next control.State) (control.State, error) {
	if err := next.Validate(); err != nil {
		return control.State{}, fmt.Errorf("validate state: %w", err)
	}
	return s.withStateTx(ctx, expected, func(tx *sql.Tx, current control.State) (control.State, error) {
		if next.LeaseFence != current.LeaseFence {
			return control.State{}, ErrStaleFence
		}
		next.Version = current.Version + 1
		next.UpdatedAt = s.now().UTC()
		if err := writeState(ctx, tx, next); err != nil {
			return control.State{}, err
		}
		return next, nil
	})
}

func (s *Store) RotateFenceAndCloseAdmission(ctx context.Context, expected uint64) (control.State, error) {
	return s.withStateTx(ctx, expected, func(tx *sql.Tx, state control.State) (control.State, error) {
		state.LeaseFence.Epoch++
		state.Admission = control.AdmissionClosed
		state.Version++
		state.UpdatedAt = s.now().UTC()
		if err := writeState(ctx, tx, state); err != nil {
			return control.State{}, err
		}
		return state, nil
	})
}

func (s *Store) RotateIncarnation(ctx context.Context) (control.State, error) {
	incarnation, err := s.uuid()
	if err != nil {
		return control.State{}, err
	}
	return s.stateTx(ctx, func(tx *sql.Tx, state control.State) (control.State, error) {
		return s.rotateIncarnation(ctx, tx, state, incarnation)
	})
}

func (s *Store) rotateIncarnation(ctx context.Context, tx *sql.Tx, state control.State, incarnation string) (control.State, error) {
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
	return state, nil
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

func (s *Store) withTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) stateTx(ctx context.Context, fn func(tx *sql.Tx, cur control.State) (control.State, error)) (control.State, error) {
	var next control.State
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		current, err := readState(ctx, tx)
		if err != nil {
			return err
		}
		next, err = fn(tx, current)
		return err
	})
	if err != nil {
		return control.State{}, err
	}
	return next, nil
}

func (s *Store) withStateTx(ctx context.Context, expected uint64, fn func(tx *sql.Tx, cur control.State) (control.State, error)) (control.State, error) {
	return s.stateTx(ctx, func(tx *sql.Tx, cur control.State) (control.State, error) {
		if cur.Version != expected {
			return control.State{}, ErrVersionConflict
		}
		return fn(tx, cur)
	})
}

type querier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readState(ctx context.Context, q querier) (control.State, error) {
	state, err := readControlState(ctx, q)
	if err != nil {
		return state, err
	}
	policy, err := readPolicyState(ctx, q)
	if err != nil {
		return control.State{}, err
	}
	state.IdlePolicy = policy.Policy
	return state, state.Validate()
}

// readControlState reads only the control_state singleton. Restore preflight
// runs it before pending migrations apply, so it must not depend on tables
// introduced after the restored backup was taken.
func readControlState(ctx context.Context, q querier) (control.State, error) {
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

func formatTime(t time.Time) string         { return t.UTC().Format(time.RFC3339Nano) }
func parseTime(v string) (time.Time, error) { return time.Parse(time.RFC3339Nano, v) }
func nullable(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// AcquireUserExecution coordinates unregistered user requests with explicit
// ownership commands. The gate is held until forwarding or the command ends.
func (s *Store) AcquireUserExecution(ctx context.Context, shared bool) (*lock.File, error) {
	return lock.AcquireContext(ctx, s.userExecutionLock, shared)
}

// Create privately before SQLite can open the file. Exclusive creation refuses
// races; existing files are revalidated and opened without following symlinks.
func secureStateFile(path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		if err := preparePath(path); err != nil {
			return err
		}
		fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		file = os.NewFile(uintptr(fd), path)
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() {
			file.Close()
			return errors.New("database path must be a regular file")
		}
	} else if err != nil {
		return err
	}
	defer file.Close()
	return file.Chmod(0o600)
}
