package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

type retentionPrunerFunc func(context.Context, time.Time, int) (int64, error)

func (fn retentionPrunerFunc) PruneCompletedWork(ctx context.Context, before time.Time, limit int) (int64, error) {
	return fn(ctx, before, limit)
}

func TestPruneExpiredWorkDrainsInBoundedBatches(t *testing.T) {
	ctx := context.Background()
	cutoff := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	calls := 0
	pruner := retentionPrunerFunc(func(_ context.Context, before time.Time, limit int) (int64, error) {
		if !before.Equal(cutoff) || limit != completedWorkPruneBatch {
			t.Fatalf("cutoff=%s, limit=%d", before, limit)
		}
		calls++
		if calls == 1 {
			return completedWorkPruneBatch, nil
		}
		return 2, nil
	})
	if err := pruneExpiredWork(ctx, pruner, cutoff); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("prune calls = %d", calls)
	}
}

func TestMaintainCompletedWorkRetriesAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	retried := make(chan struct{}, 1)
	failures := make(chan error, 1)
	pruner := retentionPrunerFunc(func(_ context.Context, _ time.Time, _ int) (int64, error) {
		if calls.Add(1) == 1 {
			return 0, errors.New("temporary sqlite failure")
		}
		select {
		case retried <- struct{}{}:
		default:
		}
		return 0, nil
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		maintainCompletedWork(ctx, pruner, 24*time.Hour, 10*time.Millisecond, func(err error) {
			failures <- err
		})
	}()
	select {
	case err := <-failures:
		if err.Error() != "temporary sqlite failure" {
			t.Fatalf("reported error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("initial maintenance did not run")
	}
	select {
	case <-retried:
	case <-time.After(time.Second):
		t.Fatal("maintenance did not retry on interval")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("maintenance did not stop after cancellation")
	}
}

func TestMaintainCompletedWorkCancelsRunningBatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	failed := make(chan error, 1)
	pruner := retentionPrunerFunc(func(ctx context.Context, _ time.Time, _ int) (int64, error) {
		close(started)
		<-ctx.Done()
		return 0, ctx.Err()
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		maintainCompletedWork(ctx, pruner, 24*time.Hour, time.Hour, func(err error) {
			failed <- err
		})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("initial maintenance did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("maintenance did not stop during running batch")
	}
	select {
	case err := <-failed:
		t.Fatalf("cancellation was reported as cleanup failure: %v", err)
	default:
	}
}

func TestPruneExpiredWorkDrainsCancelledDemandBacklog(t *testing.T) {
	ctx := context.Background()
	created := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	stateStore, err := store.OpenWithClock(ctx, filepath.Join(t.TempDir(), "state.db"), func() time.Time { return created })
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()
	for i := 0; i < completedWorkPruneBatch+1; i++ {
		demand := store.Demand{RequestID: fmt.Sprintf("cancelled-%d", i), Requester: "requester", Workflow: "workflow", BodySHA256: strings.Repeat("a", 64), Workload: control.WorkloadMedia}
		if err := stateStore.QueueDemand(ctx, demand); err != nil {
			t.Fatal(err)
		}
		if err := stateStore.CancelDemand(ctx, demand); err != nil {
			t.Fatal(err)
		}
	}
	var batches int
	var deleted int64
	pruner := retentionPrunerFunc(func(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
		batches++
		count, err := stateStore.PruneCompletedWork(ctx, cutoff, limit)
		deleted += count
		return count, err
	})
	if err := pruneExpiredWork(ctx, pruner, created.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if batches != 2 || deleted != completedWorkPruneBatch+1 {
		t.Fatalf("batches=%d deleted=%d", batches, deleted)
	}
	if remaining, err := stateStore.PruneCompletedWork(ctx, created.Add(time.Hour), completedWorkPruneBatch); err != nil || remaining != 0 {
		t.Fatalf("backlog remaining=%d %v", remaining, err)
	}
}
