package store

import (
	"context"
	"database/sql"
)

// touchActivity records activity time monotonically: concurrent finishers can
// never move last_activity_at backwards. String timestamps compare incorrectly
// at subsecond precision, so the comparison goes through julianday. Activity
// invalidates any armed deadline computed from the previous activity marker,
// so the armed fields are cleared in the same update; the read model then
// never reports a stale pending deadline.
func (s *Store) touchActivity(ctx context.Context, tx *sql.Tx) error {
	now := formatTime(s.now())
	return updateSingleton(ctx, tx, `UPDATE idle_policy_state
		SET last_activity_at = CASE
			WHEN last_activity_at IS NULL OR julianday(?) > julianday(last_activity_at) THEN ?
			ELSE last_activity_at END,
		armed_deadline = NULL, attestation_at = NULL,
		updated_at = ?
		WHERE singleton = 1`, now, now, now)
}

func pendingWorkTx(ctx context.Context, tx *sql.Tx) (int, error) {
	var pending int
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM registered_work WHERE completed_at IS NULL)`).Scan(&pending)
	return pending, err
}
