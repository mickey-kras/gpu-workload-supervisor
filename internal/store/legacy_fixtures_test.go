package store

// Legacy entry points exist only for interrupted-state and old-schema fixtures.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func (s *Store) RegisterWork(ctx context.Context, requestID, jobID string, workload control.Workload, fence control.Fence) error {
	tx, err := s.beginAdmittedWorkAtCatalog(ctx, requestID, workload, fence, nil)
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

func (s *Store) AdmitWork(ctx context.Context, requestID, jobID string, workload control.Workload, fence control.Fence) error {
	return s.admitWork(ctx, requestID, jobID, workload, fence, "")
}

func (s *Store) FinishWorkFenced(ctx context.Context, requestID string, workload control.Workload, fence control.Fence, outcome WorkOutcome) error {
	return s.FinishWorkToken(ctx, requestID, workload, fence, "", outcome)
}
