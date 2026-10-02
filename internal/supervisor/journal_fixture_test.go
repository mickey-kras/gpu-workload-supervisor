package supervisor

import (
	"context"
	"database/sql"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
	"time"
)

func transitionEvents(s *store.Store, ctx context.Context, transitionID string) ([]store.TransitionEvent, error) {
	db, err := sql.Open("sqlite", s.DurableStatePath())
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT sequence, transition_id, phase, kind, action,
		COALESCE(outcome, ''), created_at FROM transition_events
		WHERE transition_id = ? ORDER BY sequence`, transitionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []store.TransitionEvent
	for rows.Next() {
		var event store.TransitionEvent
		var created string
		if err := rows.Scan(&event.Sequence, &event.TransitionID, &event.Phase, &event.Kind,
			&event.Action, &event.Outcome, &created); err != nil {
			return nil, err
		}
		event.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}
