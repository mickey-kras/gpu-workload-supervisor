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

var (
	ErrUnstableState     = errors.New("conditional transition requires stable non-error state")
	ErrTransitionRunning = errors.New("conditional transition already running")
)

func (s *Store) StartTransition(ctx context.Context, expected uint64, tr Transition) (control.State, error) {
	return s.startTransition(ctx, expected, nil, tr)
}

func (s *Store) startTransition(ctx context.Context, expected uint64, operator *control.OperatorPrecondition, tr Transition) (control.State, error) {
	if tr.ID == "" {
		return control.State{}, errors.New("transition id is empty")
	}
	return s.withStateTx(ctx, expected, func(tx *sql.Tx, current control.State) (control.State, error) {
		if err := transitionOperatorSource(ctx, tx, current, operator); err != nil {
			return control.State{}, err
		}
		if err := validateTransitionCatalog(ctx, tx, tr); err != nil {
			return control.State{}, err
		}
		return s.beginTransitionTx(ctx, tx, current, tr)
	})
}

// beginTransitionTx persists the draining entry. Every transition disarms the
// verified idle deadline in the same transaction: the next policy tick must
// re-attest and re-arm before idling.
func (s *Store) beginTransitionTx(ctx context.Context, tx *sql.Tx, current control.State, tr Transition) (control.State, error) {
	next := current
	next.DesiredWorkload = tr.Target.DesiredWorkload
	next.Admission = control.AdmissionClosed
	next.Phase = control.PhaseDraining
	next.LeaseFence.Epoch++
	next.Version++
	next.UpdatedAt = s.now().UTC()
	if err := s.disarmIdleDeadline(ctx, tx); err != nil {
		return control.State{}, err
	}
	next.PendingIdleDeadline = nil
	if err := next.Validate(); err != nil {
		return control.State{}, err
	}
	if err := s.recordTransition(ctx, tx, current, next, tr); err != nil {
		return control.State{}, err
	}
	if err := writeState(ctx, tx, next); err != nil {
		return control.State{}, err
	}
	return next, nil
}

func (s *Store) recordTransition(ctx context.Context, tx *sql.Tx, current, next control.State, tr Transition) error {
	source, err := json.Marshal(current)
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO transitions
		(transition_id, lease_incarnation, lease_epoch, source_state, target_state,
		 previous_state, initiator, job_id, phase, deadline, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'in_progress', ?, ?)`,
		tr.ID, next.LeaseFence.Incarnation, next.LeaseFence.Epoch, source, target, previous,
		tr.Initiator, nullable(tr.JobID), next.Phase, formatTime(tr.Deadline), now, now); err != nil {
		return fmt.Errorf("insert transition: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO transition_work(transition_id, request_id)
		SELECT ?, request_id FROM registered_work WHERE completed_at IS NULL`, tr.ID); err != nil {
		return fmt.Errorf("snapshot work: %w", err)
	}
	return nil
}

func updateRunningTransition(ctx context.Context, tx *sql.Tx, query string, args ...any) error {
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrTransitionNotRunning
	}
	return nil
}

func (s *Store) SetTransitionPhase(ctx context.Context, transitionID string, expected uint64, phase control.Phase) (control.State, error) {
	return s.withStateTx(ctx, expected, func(tx *sql.Tx, state control.State) (control.State, error) {
		state.Phase = phase
		state.Admission = control.AdmissionClosed
		state.PendingIdleDeadline = nil
		state.Version++
		state.UpdatedAt = s.now().UTC()
		if err := state.Validate(); err != nil {
			return control.State{}, err
		}
		if err := updateRunningTransition(ctx, tx, `UPDATE transitions SET phase = ?, updated_at = ?
			WHERE transition_id = ? AND status = 'in_progress'`,
			phase, formatTime(s.now()), transitionID); err != nil {
			return control.State{}, err
		}
		if err := writeState(ctx, tx, state); err != nil {
			return control.State{}, err
		}
		return state, nil
	})
}

func (s *Store) FinishTransition(ctx context.Context, transitionID, status string, expected uint64, final control.State) (control.State, error) {
	if status != "committed" && status != "failed" {
		return control.State{}, errors.New("transition status must be committed or failed")
	}
	return s.withStateTx(ctx, expected, func(tx *sql.Tx, current control.State) (control.State, error) {
		if final.LeaseFence != current.LeaseFence {
			return control.State{}, ErrStaleFence
		}
		final.PendingIdleDeadline = nil
		final.Version = current.Version + 1
		final.UpdatedAt = s.now().UTC()
		if err := final.Validate(); err != nil {
			return control.State{}, err
		}
		if status == "committed" && final.ActiveWorkload != control.WorkloadIdle {
			if err := s.touchActivity(ctx, tx); err != nil {
				return control.State{}, err
			}
		}
		if err := updateRunningTransition(ctx, tx, `UPDATE transitions SET phase = ?, status = ?, updated_at = ?
			WHERE transition_id = ? AND status = 'in_progress'`,
			final.Phase, status, formatTime(s.now()), transitionID); err != nil {
			return control.State{}, err
		}
		if err := writeState(ctx, tx, final); err != nil {
			return control.State{}, err
		}
		return final, nil
	})
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
	return s.withStateTx(ctx, expected, func(tx *sql.Tx, current control.State) (control.State, error) {
		final.LeaseFence = current.LeaseFence
		final.LeaseFence.Epoch++
		final.Version = current.Version + 1
		final.UpdatedAt = s.now().UTC()
		if err := s.disarmIdleDeadline(ctx, tx); err != nil {
			return control.State{}, err
		}
		final.PendingIdleDeadline = nil
		if err := final.Validate(); err != nil {
			return control.State{}, err
		}
		if err := s.failInProgressTransition(ctx, tx, final.Phase, reason); err != nil {
			return control.State{}, err
		}
		if err := writeState(ctx, tx, final); err != nil {
			return control.State{}, err
		}
		return final, nil
	})
}

func (s *Store) failInProgressTransition(ctx context.Context, tx *sql.Tx, phase control.Phase, reason string) error {
	var transitionID string
	err := tx.QueryRowContext(ctx, `SELECT transition_id FROM transitions
		WHERE status = 'in_progress' ORDER BY created_at, transition_id LIMIT 1`).Scan(&transitionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := updateRunningTransition(ctx, tx, `UPDATE transitions
		SET phase = ?, status = 'failed', updated_at = ?
		WHERE transition_id = ? AND status = 'in_progress'`,
		phase, formatTime(s.now()), transitionID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO transition_events
		(transition_id, phase, kind, action, outcome, created_at)
		VALUES (?, ?, 'observation', 'recovery', ?, ?)`,
		transitionID, phase, reason, formatTime(s.now()))
	return err
}

func validateTransitionCatalog(ctx context.Context, tx *sql.Tx, tr Transition) error {
	catalog, err := readCatalog(ctx, tx)
	if err != nil {
		return err
	}
	if catalog.Revision != tr.ConfigurationRevision {
		return ErrVersionConflict
	}
	if tr.Target.DesiredWorkload != control.WorkloadIdle {
		if err := catalogAdmitsWorkload(catalog, tr.Target.DesiredWorkload); err != nil {
			return err
		}
	}
	return nil
}

func transitionOperatorSource(ctx context.Context, tx *sql.Tx, current control.State, operator *control.OperatorPrecondition) error {
	if operator == nil {
		return nil
	}
	return operatorSource(ctx, tx, current, *operator)
}
