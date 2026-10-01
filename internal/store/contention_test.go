package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"modernc.org/sqlite"
)

// Pausing the existing clock puts the transaction after its admission/state
// read but before its first write. A second Store models the proxy process;
// a per-Store connection limit cannot serialize these independent writers.
func TestWritesReserveSnapshotBeforeConcurrentRetention(t *testing.T) {
	for _, operation := range []string{"admission", "transition phase", "state update"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			s := testStore(t)
			other, err := Open(ctx, s.DurableStatePath())
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			state, err := s.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			state.Phase = control.PhaseStable
			state.Admission = control.AdmissionOpen
			state.ActiveWorkload = control.WorkloadText
			state.DesiredWorkload = control.WorkloadText
			state, err = s.UpdateState(ctx, state.Version, state)
			if err != nil {
				t.Fatal(err)
			}
			token, err := s.AdmitWorkToken(ctx, "old", "", control.WorkloadText, state.LeaseFence)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.FinishWorkToken(ctx, "old", control.WorkloadText, state.LeaseFence, token, WorkCompleted); err != nil {
				t.Fatal(err)
			}
			if operation == "transition phase" {
				state, err = s.StartTransition(ctx, state.Version, Transition{ID: "transition", Target: state, Previous: state, Deadline: fixedClock()().Add(time.Minute)})
				if err != nil {
					t.Fatal(err)
				}
			}
			// Fail competing writes immediately, so scheduling and sleeps are not
			// needed to prove that the transaction has already reserved the writer.
			if _, err := other.db.Exec("PRAGMA busy_timeout = 0"); err != nil {
				t.Fatal(err)
			}
			reached, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			var pause sync.Once
			s.now = func() time.Time { pause.Do(func() { close(reached); <-release }); return fixedClock()() }
			done := make(chan error, 1)
			go func() {
				var err error
				switch operation {
				case "admission":
					err = s.AdmitWork(ctx, "new", "", control.WorkloadText, state.LeaseFence)
				case "transition phase":
					_, err = s.SetTransitionPhase(ctx, "transition", state.Version, control.PhaseUnloading)
				case "state update":
					_, err = s.UpdateState(ctx, state.Version, state)
				}
				done <- err
			}()
			select {
			case <-reached:
			case err := <-done:
				t.Fatalf("write returned before read barrier: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("write did not reach read barrier")
			}
			// WAL readers must remain available while the transaction reserves writing.
			if got, err := other.State(ctx); err != nil || got.Version != state.Version {
				t.Errorf("concurrent state read = %#v, %v", got, err)
			}
			readTx, err := other.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
			if err != nil {
				t.Errorf("read-only transaction blocked by writer: %v", err)
			} else {
				if got, err := readState(ctx, readTx); err != nil || got.Version != state.Version {
					t.Errorf("transactional concurrent read = %#v, %v", got, err)
				}
				if err := readTx.Rollback(); err != nil {
					t.Error(err)
				}
			}
			count, pruneErr := other.PruneCompletedWork(ctx, fixedClock()().Add(time.Hour), 100)
			unblock()
			if err := <-done; err != nil {
				t.Errorf("write lost its snapshot: %v", err)
			}
			var sqliteErr *sqlite.Error
			if count != 0 || !errors.As(pruneErr, &sqliteErr) || sqliteErr.Code() != 5 {
				t.Errorf("retention bypassed reserved writer: count=%d error=%v", count, pruneErr)
			}
			if count, err := other.PruneCompletedWork(ctx, fixedClock()().Add(time.Hour), 100); err != nil || count != 1 {
				t.Errorf("retention after commit = %d, %v", count, err)
			}
			got, err := other.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if operation == "transition phase" && (got.Phase != control.PhaseUnloading || got.Health != control.HealthHealthy) {
				t.Errorf("phase write not committed: %#v", got)
			}
			if operation == "admission" {
				var count int
				if err := other.db.QueryRow("SELECT COUNT(*) FROM registered_work WHERE request_id = 'new'").Scan(&count); err != nil || count != 1 {
					t.Errorf("admission not committed: %d, %v", count, err)
				}
			}
		})
	}
}

func TestStorePathRetainsURICharacters(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state ?#% database.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	original, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenRestored(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got, err := s.State(ctx); err != nil || got.LeaseFence != original.LeaseFence {
		t.Fatalf("restored state = %#v, %v", got, err)
	}
}

func TestCanceledWriteWaitingForOtherStoreDoesNotCommit(t *testing.T) {
	ctx := context.Background()
	holder := testStore(t)
	waiter, err := Open(ctx, holder.DurableStatePath())
	if err != nil {
		t.Fatal(err)
	}
	defer waiter.Close()
	state, err := waiter.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := holder.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	// This explicit write also makes the baseline deferred transaction hold a
	// writer lock, so this test covers cancellation independent of begin mode.
	if _, err := tx.ExecContext(ctx, "UPDATE control_state SET version = version WHERE singleton = 1"); err != nil {
		t.Fatal(err)
	}
	deadline, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = waiter.UpdateState(deadline, state.Version, state)
	if err == nil {
		t.Fatal("blocked write succeeded after deadline")
	}
	// modernc's BEGIN uses sqlite3_exec: cancellation cannot interrupt its
	// busy handler immediately. Preserve the existing five-second busy bound
	// (with scheduling margin), and ensure a canceled operation never commits.
	if elapsed := time.Since(start); elapsed > 7*time.Second {
		t.Fatalf("canceled writer exceeded busy timeout: %v", elapsed)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	got, err := waiter.State(ctx)
	if err != nil || got.Version != state.Version {
		t.Fatalf("canceled write changed state: %#v, %v", got, err)
	}
	if _, err := waiter.UpdateState(ctx, state.Version, state); err != nil {
		t.Fatalf("connection unusable after cancellation: %v", err)
	}
}
