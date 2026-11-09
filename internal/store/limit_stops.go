package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

// schemaV19 adds the limit_stops table. The limit stop of the policy tick
// writes it, and each state write that ends the latch removes its row.
// Each time is an INTEGER of Unix nanoseconds in UTC, as
// time.Time.UnixNano gives it, so the CHECKs compare numbers. The store's
// RFC 3339 text has no fixed width, so it cannot keep this order. A stop
// needs a definite time, and the times keep their order: first seen,
// definite, stopping. SQLite cannot add a CHECK to an existing table, so
// these rules are part of v19.
const schemaV19 = `CREATE TABLE limit_stops(
 singleton INTEGER PRIMARY KEY CHECK(singleton=1),
 workload TEXT NOT NULL CHECK(length(workload) BETWEEN 1 AND 64 AND workload NOT IN ('idle','unknown')),
 first_seen_at INTEGER NOT NULL CHECK(typeof(first_seen_at)='integer' AND first_seen_at > 0),
 definite_at INTEGER CHECK(definite_at IS NULL OR typeof(definite_at)='integer' AND definite_at >= first_seen_at),
 stopping_at INTEGER CHECK(stopping_at IS NULL OR typeof(stopping_at)='integer' AND definite_at IS NOT NULL AND stopping_at >= definite_at));`

// LimitStop is the stored limit stop of one unit. A zero time is absent.
type LimitStop struct {
	Workload    control.Workload
	FirstSeenAt time.Time
	DefiniteAt  time.Time
	StoppingAt  time.Time
}

// ErrLimitStopStarted marks a failure record of another workload while a
// started stop holds the record. The started stop is never replaced.
var ErrLimitStopStarted = errors.New("a started limit stop of another workload holds the record")

// ErrNoLimitFailure marks a stop record of a workload without a stored
// failure.
var ErrNoLimitFailure = errors.New("no limit failure is recorded for this workload")

// LimitStop reads the stored limit stop. ok is false when there is none.
func (s *Store) LimitStop(ctx context.Context) (stop LimitStop, ok bool, err error) {
	var workload string
	var firstSeen int64
	var definite, stopping sql.NullInt64
	err = s.db.QueryRowContext(ctx, `SELECT workload, first_seen_at, definite_at, stopping_at FROM limit_stops WHERE singleton=1`).
		Scan(&workload, &firstSeen, &definite, &stopping)
	if errors.Is(err, sql.ErrNoRows) {
		return LimitStop{}, false, nil
	}
	if err != nil {
		return LimitStop{}, false, err
	}
	stop = LimitStop{Workload: control.Workload(workload), FirstSeenAt: time.Unix(0, firstSeen).UTC()}
	if definite.Valid {
		stop.DefiniteAt = time.Unix(0, definite.Int64).UTC()
	}
	if stopping.Valid {
		stop.StoppingAt = time.Unix(0, stopping.Int64).UTC()
	}
	return stop, true, nil
}

// MarkLimitFailure records a failed check of workload at at and returns
// the stored record. definite also records the first definite failure.
// The times belong to the latch, not to one workload: a record of another
// workload passes to workload, but it keeps the earlier first time and
// any definite time. So units that fail in turn cannot reset the times.
// A definite time before the stored first time is set to the first time,
// so the v19 CHECKs hold when the clock goes back. A record of another
// workload with a started stop stays, and the call fails with
// ErrLimitStopStarted.
func (s *Store) MarkLimitFailure(ctx context.Context, workload control.Workload, definite bool, at time.Time) (LimitStop, error) {
	now := at.UnixNano()
	var definiteAt any
	if definite {
		definiteAt = now
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO limit_stops(singleton, workload, first_seen_at, definite_at) VALUES(1, ?, ?, ?)
		ON CONFLICT(singleton) DO UPDATE SET
		 first_seen_at = MIN(first_seen_at, CASE WHEN workload = excluded.workload THEN first_seen_at ELSE excluded.first_seen_at END),
		 definite_at = COALESCE(definite_at, MAX(excluded.definite_at,
		  MIN(first_seen_at, CASE WHEN workload = excluded.workload THEN first_seen_at ELSE excluded.first_seen_at END))),
		 stopping_at = CASE WHEN workload = excluded.workload THEN stopping_at ELSE NULL END,
		 workload = excluded.workload
		WHERE stopping_at IS NULL OR workload = excluded.workload`, string(workload), now, definiteAt)
	if err != nil {
		return LimitStop{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return LimitStop{}, fmt.Errorf("read affected rows: %w", err)
	}
	if changed != 1 {
		return LimitStop{}, ErrLimitStopStarted
	}
	stop, _, err := s.LimitStop(ctx)
	return stop, err
}

// MarkLimitStopping records at as the start of the stop of workload. It is
// written before the stop, so a later run can finish it. A record with no
// definite time gets at as its definite time: an overdue failure that
// proves nothing is stopped like a definite one. Each time is at least the
// time before it, so the v19 CHECKs hold when the clock goes back.
func (s *Store) MarkLimitStopping(ctx context.Context, workload control.Workload, at time.Time) error {
	now := at.UnixNano()
	result, err := s.db.ExecContext(ctx, `UPDATE limit_stops SET definite_at = COALESCE(definite_at, MAX(?, first_seen_at)),
		stopping_at = MAX(?, COALESCE(definite_at, first_seen_at)) WHERE singleton=1 AND workload = ?`, now, now, string(workload))
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read affected rows: %w", err)
	}
	if changed != 1 {
		return ErrNoLimitFailure
	}
	return nil
}

// ClearLimitStop removes the stored limit stop.
func (s *Store) ClearLimitStop(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM limit_stops WHERE singleton=1`)
	return err
}

// PendingWorkFor returns 1 when unfinished work of one workload exists, and
// 0 otherwise. Work without a workload identity counts too, because it
// cannot be attributed elsewhere.
func (s *Store) PendingWorkFor(ctx context.Context, workload control.Workload) (int, error) {
	var pending int
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM registered_work WHERE completed_at IS NULL AND (workload IS NULL OR workload = ?))`, string(workload)).Scan(&pending)
	return pending, err
}

// clearUnlatchedLimitStop removes the limit stop record when a state write
// ends the latch. The record belongs to one latch, so a later latch starts
// with new times. It runs in the transaction of the state write.
func clearUnlatchedLimitStop(ctx context.Context, tx *sql.Tx, state control.State) error {
	if state.Health == control.HealthError {
		return nil
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM limit_stops WHERE singleton=1`)
	return err
}
