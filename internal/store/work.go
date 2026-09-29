package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func (s *Store) CompleteWorkFenced(ctx context.Context, requestID string, fence control.Fence) error {
	if requestID == "" {
		return errors.New("request id is empty")
	}
	if err := fence.Validate(); err != nil {
		return fmt.Errorf("invalid fence: %w", err)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE registered_work SET completed_at = ?
		WHERE request_id = ? AND completed_at IS NULL
		  AND lease_incarnation = ? AND lease_epoch = ?`,
		formatTime(s.now()), requestID, fence.Incarnation, fence.Epoch)
	if err != nil {
		return fmt.Errorf("complete work: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 1 {
		return nil
	}
	var incarnation string
	var epoch uint64
	err = s.db.QueryRowContext(ctx, `SELECT lease_incarnation, lease_epoch
		FROM registered_work WHERE request_id = ?`, requestID).Scan(&incarnation, &epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return sql.ErrNoRows
	}
	if err != nil {
		return err
	}
	if incarnation != fence.Incarnation || epoch != fence.Epoch {
		return ErrStaleFence
	}
	return sql.ErrNoRows
}
