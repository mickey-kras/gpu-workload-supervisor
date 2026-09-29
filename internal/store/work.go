package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

var ErrRequestConflict = errors.New("request id already exists")

type WorkOutcome string

const (
	WorkCompleted WorkOutcome = "completed"
	WorkAbandoned WorkOutcome = "abandoned"
)

func (s *Store) AdmitWork(ctx context.Context, requestID, jobID string, workload control.Workload, fence control.Fence) error {
	if requestID == "" {
		return errors.New("request id is empty")
	}
	if workload != control.WorkloadText && workload != control.WorkloadMedia {
		return errors.New("workload must be text or media")
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
	if state.Admission != control.AdmissionOpen || state.Phase != control.PhaseStable || state.Health != control.HealthHealthy {
		return ErrAdmissionClosed
	}
	if state.ActiveWorkload != workload || state.DesiredWorkload != workload {
		return ErrWorkloadMismatch
	}
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO registered_work
		(request_id, job_id, workload, lease_incarnation, lease_epoch, registered_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		requestID, nullable(jobID), workload, fence.Incarnation, fence.Epoch, formatTime(s.now()))
	if err != nil {
		return fmt.Errorf("admit work: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrRequestConflict
	}
	return tx.Commit()
}

func (s *Store) FinishWorkFenced(ctx context.Context, requestID string, workload control.Workload, fence control.Fence, outcome WorkOutcome) error {
	if requestID == "" {
		return errors.New("request id is empty")
	}
	if err := fence.Validate(); err != nil {
		return fmt.Errorf("invalid fence: %w", err)
	}
	if workload != control.WorkloadText && workload != control.WorkloadMedia {
		return errors.New("workload must be text or media")
	}
	if outcome != WorkCompleted && outcome != WorkAbandoned {
		return errors.New("invalid work outcome")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE registered_work
		SET completed_at = ?, completion_outcome = ?
		WHERE request_id = ? AND completed_at IS NULL
		  AND workload = ? AND lease_incarnation = ? AND lease_epoch = ?`,
		formatTime(s.now()), outcome, requestID, workload, fence.Incarnation, fence.Epoch)
	if err != nil {
		return fmt.Errorf("finish work: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 1 {
		return nil
	}
	var registeredWorkload control.Workload
	var incarnation string
	var epoch uint64
	err = s.db.QueryRowContext(ctx, `SELECT workload, lease_incarnation, lease_epoch
		FROM registered_work WHERE request_id = ?`, requestID).Scan(&registeredWorkload, &incarnation, &epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return sql.ErrNoRows
	}
	if err != nil {
		return err
	}
	if registeredWorkload != workload {
		return ErrWorkloadMismatch
	}
	if incarnation != fence.Incarnation || epoch != fence.Epoch {
		return ErrStaleFence
	}
	return sql.ErrNoRows
}

