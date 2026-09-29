package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

var ErrTransitionNotRunning = errors.New("transition is not running")

func (s *Store) StartTransition(ctx context.Context, expected uint64, tr Transition) (control.State, error) {
	if tr.ID == "" {
		return control.State{}, errors.New("transition id is empty")
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
	next := current
	next.DesiredWorkload = tr.Target.DesiredWorkload
	next.Admission = control.AdmissionClosed
	next.Phase = control.PhaseDraining
	next.LeaseFence.Epoch++
	next.Version++
	next.UpdatedAt = s.now().UTC()
	if err := next.Validate(); err != nil {
		return control.State{}, err
	}
	source, err := json.Marshal(current)
	if err != nil {
		return control.State{}, err
	}
	target, err := json.Marshal(tr.Target)
	if err != nil {
		return control.State{}, err
	}
	previous, err := json.Marshal(tr.Previous)
	if err != nil {
		return control.State{}, err
	}
	now := formatTime(s.now())
	_, err = tx.ExecContext(ctx, `INSERT INTO transitions
		(transition_id, lease_incarnation, lease_epoch, source_state, target_state,
		 previous_state, initiator, job_id, phase, deadline, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'in_progress', ?, ?)`,
		tr.ID, next.LeaseFence.Incarnation, next.LeaseFence.Epoch, source, target, previous,
		tr.Initiator, nullable(tr.JobID), next.Phase, formatTime(tr.Deadline), now, now)
	if err != nil {
		return control.State{}, fmt.Errorf("insert transition: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO transition_work(transition_id, request_id)
		SELECT ?, request_id FROM registered_work WHERE completed_at IS NULL`, tr.ID); err != nil {
		return control.State{}, fmt.Errorf("snapshot work: %w", err)
	}
	if err := writeState(ctx, tx, next); err != nil {
		return control.State{}, err
	}
	if err := tx.Commit(); err != nil {
		return control.State{}, err
	}
	return next, nil
}

func (s *Store) SetTransitionPhase(ctx context.Context, transitionID string, expected uint64, phase control.Phase) (control.State, error) {
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
	state.Phase = phase
	state.Admission = control.AdmissionClosed
	state.Version++
	state.UpdatedAt = s.now().UTC()
	if err := state.Validate(); err != nil {
		return control.State{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE transitions SET phase = ?, updated_at = ?
		WHERE transition_id = ? AND status = 'in_progress'`,
		phase, formatTime(s.now()), transitionID)
	if err != nil {
		return control.State{}, err
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		if err != nil {
			return control.State{}, err
		}
		return control.State{}, ErrTransitionNotRunning
	}
	if err := writeState(ctx, tx, state); err != nil {
		return control.State{}, err
	}
	if err := tx.Commit(); err != nil {
		return control.State{}, err
	}
	return state, nil
}

func (s *Store) FinishTransition(ctx context.Context, transitionID, status string, expected uint64, final control.State) (control.State, error) {
	if status != "committed" && status != "failed" {
		return control.State{}, errors.New("transition status must be committed or failed")
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
	if final.LeaseFence != current.LeaseFence {
		return control.State{}, ErrStaleFence
	}
	final.Version = current.Version + 1
	final.UpdatedAt = s.now().UTC()
	if err := final.Validate(); err != nil {
		return control.State{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE transitions SET phase = ?, status = ?, updated_at = ?
		WHERE transition_id = ? AND status = 'in_progress'`,
		final.Phase, status, formatTime(s.now()), transitionID)
	if err != nil {
		return control.State{}, err
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		if err != nil {
			return control.State{}, err
		}
		return control.State{}, ErrTransitionNotRunning
	}
	if err := writeState(ctx, tx, final); err != nil {
		return control.State{}, err
	}
	if err := tx.Commit(); err != nil {
		return control.State{}, err
	}
	return final, nil
}

func (s *Store) PendingTransitionWork(ctx context.Context, transitionID string) (int, error) {
	var pending int
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1
		FROM transition_work AS snapshot
		JOIN registered_work AS work ON work.request_id = snapshot.request_id
		WHERE snapshot.transition_id = ? AND work.completed_at IS NULL)`, transitionID).Scan(&pending)
	return pending, err
}

func (s *Store) InProgressTransition(ctx context.Context) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT transition_id FROM transitions
		WHERE status = 'in_progress' ORDER BY created_at, transition_id LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func (s *Store) Recover(ctx context.Context, expected uint64, final control.State, reason string) (control.State, error) {
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
	final.LeaseFence = current.LeaseFence
	final.LeaseFence.Epoch++
	final.Version = current.Version + 1
	final.UpdatedAt = s.now().UTC()
	if err := final.Validate(); err != nil {
		return control.State{}, err
	}
	var transitionID string
	err = tx.QueryRowContext(ctx, `SELECT transition_id FROM transitions
		WHERE status = 'in_progress' ORDER BY created_at, transition_id LIMIT 1`).Scan(&transitionID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return control.State{}, err
	}
	if err == nil {
		result, updateErr := tx.ExecContext(ctx, `UPDATE transitions
			SET phase = ?, status = 'failed', updated_at = ?
			WHERE transition_id = ? AND status = 'in_progress'`,
			final.Phase, formatTime(s.now()), transitionID)
		if updateErr != nil {
			return control.State{}, updateErr
		}
		if changed, rowsErr := result.RowsAffected(); rowsErr != nil || changed != 1 {
			if rowsErr != nil {
				return control.State{}, rowsErr
			}
			return control.State{}, ErrTransitionNotRunning
		}
		if _, eventErr := tx.ExecContext(ctx, `INSERT INTO transition_events
			(transition_id, phase, kind, action, outcome, created_at)
			VALUES (?, ?, 'observation', 'recovery', ?, ?)`,
			transitionID, final.Phase, reason, formatTime(s.now())); eventErr != nil {
			return control.State{}, eventErr
		}
	}
	if err := writeState(ctx, tx, final); err != nil {
		return control.State{}, err
	}
	if err := tx.Commit(); err != nil {
		return control.State{}, err
	}
	return final, nil
}
