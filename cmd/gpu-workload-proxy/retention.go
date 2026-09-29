package main

import (
	"context"
	"errors"
	"time"
)

const completedWorkPruneBatch = 256

type completedWorkPruner interface {
	PruneCompletedWork(context.Context, time.Time, int) (int64, error)
}

// pruneExpiredWork uses short transactions so cleanup does not hold the store
// while the proxy handles requests. It drains a backlog in bounded batches.
func pruneExpiredWork(ctx context.Context, pruner completedWorkPruner, cutoff time.Time) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		batchCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		count, err := pruner.PruneCompletedWork(batchCtx, cutoff, completedWorkPruneBatch)
		cancel()
		if err != nil {
			return err
		}
		if count < 0 || count > completedWorkPruneBatch {
			return errors.New("completed work prune returned an invalid count")
		}
		if count < completedWorkPruneBatch {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func maintainCompletedWork(ctx context.Context, pruner completedWorkPruner, retention, interval time.Duration, report func(error)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := pruneExpiredWork(ctx, pruner, time.Now().Add(-retention)); err != nil && ctx.Err() == nil {
			report(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
