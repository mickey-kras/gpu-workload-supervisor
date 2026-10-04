package store

import (
	"context"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"testing"
	"time"
)

func TestOperatorStartAtomic(t *testing.T) {
	for _, mode := range []string{"ok", "owner", "incarnation", "version", "revision"} {
		t.Run(mode, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			s.db.Exec(`CREATE TABLE IF NOT EXISTS workload_catalog (singleton INTEGER PRIMARY KEY, revision TEXT NOT NULL, catalog BLOB NOT NULL)`)
			s.db.Exec(`INSERT INTO workload_catalog VALUES(1,'rev','{}')`)
			state, _ := s.State(ctx)
			state.Phase = control.PhaseStable
			state.ActiveWorkload = control.WorkloadIdle
			state.Owner = control.OwnerUser
			state, _ = s.UpdateState(ctx, state.Version, state)
			e := control.OperatorPrecondition{Incarnation: state.LeaseFence.Incarnation, Version: state.Version, Owner: state.Owner, ConfigurationRevision: "rev"}
			want := error(nil)
			switch mode {
			case "owner":
				e.Owner = control.OwnerSupervisor
				want = ErrWrongOwner
			case "incarnation":
				e.Incarnation = "restored"
				want = ErrVersionConflict
			case "version":
				e.Version++
				want = ErrVersionConflict
			case "revision":
				e.ConfigurationRevision = "new"
				want = ErrConfigurationConflict
			}
			_, err := s.StartOperatorTransition(ctx, e, Transition{ID: "operator", Target: state, Previous: state, ConfigurationRevision: "rev", Deadline: time.Now().Add(time.Minute)})
			if !errors.Is(err, want) {
				t.Fatalf("%v want %v", err, want)
			}
			if want != nil {
				after, _ := s.State(ctx)
				if after != state {
					t.Fatal("rejected request mutated state")
				}
			}
		})
	}
}
