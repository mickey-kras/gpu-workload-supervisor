package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// PruneAuditHistory deletes at most limit audit parents and their dependent rows.
// It never deletes unresolved work, live transitions, current-fence transitions,
// or the latest recovery/restore record. Archive before invoking this operation.
func (s *Store) PruneAuditHistory(ctx context.Context, before time.Time, limit int) (int64, error) {
	if before.IsZero() || !before.Before(s.now()) {
		return 0, errors.New("audit cutoff must be in the past")
	}
	if limit < 1 || limit > 1024 {
		return 0, errors.New("audit batch must be between 1 and 1024")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	cutoff := formatTime(before)
	ids, err := prunableAudits(ctx, tx, cutoff, limit)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := deleteTransitionAudit(ctx, tx, id); err != nil {
			return 0, err
		}
	}
	count := int64(len(ids))
	for _, table := range []string{"state_restorations", "work_resolutions"} {
		n, err := pruneRecoveryAudits(ctx, tx, table, cutoff, int64(limit)-count)
		if err != nil {
			return 0, err
		}
		count += n
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

func prunableAudits(ctx context.Context, tx *sql.Tx, cutoff string, limit int) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT tr.transition_id FROM transitions tr JOIN control_state state ON state.singleton=1
 WHERE tr.status <> 'in_progress' AND unixepoch(tr.updated_at)<unixepoch(?)
 AND (tr.lease_incarnation<>state.lease_incarnation OR tr.lease_epoch<>state.lease_epoch)
 AND NOT EXISTS(SELECT 1 FROM transition_work tw JOIN registered_work w ON w.request_id=tw.request_id WHERE tw.transition_id=tr.transition_id AND w.completed_at IS NULL)
 AND NOT EXISTS(SELECT 1 FROM temporary_discovery_sessions s WHERE s.id=tr.transition_id)
 ORDER BY tr.updated_at,tr.transition_id LIMIT ?`, cutoff, limit)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	return ids, nil
}

func deleteTransitionAudit(ctx context.Context, tx *sql.Tx, id string) error {
	for _, query := range []string{`DELETE FROM transition_events WHERE transition_id=?`, `DELETE FROM transition_work WHERE transition_id=?`, `DELETE FROM transitions WHERE transition_id=?`} {
		if _, err := tx.ExecContext(ctx, query, id); err != nil {
			return err
		}
	}
	return nil
}
func pruneRecoveryAudits(ctx context.Context, tx *sql.Tx, table, cutoff string, remaining int64) (int64, error) {
	// Callers select fixed table names. Unresolved work conservatively pins all
	// recovery audits because there are no per-work audit links.
	result, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE sequence IN (
 SELECT sequence FROM `+table+` WHERE unixepoch(created_at)<unixepoch(?)
 AND sequence<>(SELECT MAX(sequence) FROM `+table+`)
 AND NOT EXISTS(SELECT 1 FROM registered_work WHERE completed_at IS NULL)
 AND NOT EXISTS(SELECT 1 FROM transitions WHERE status='in_progress')
 ORDER BY sequence LIMIT ?)`, cutoff, remaining)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
