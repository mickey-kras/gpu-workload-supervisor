package store

import (
	"context"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"testing"
	"time"
)

func TestConditionalStartChecksTokenAndRestrictedStateAtomically(t *testing.T) {
	for _, scenario := range []string{"incarnation", "version", "user", "phase", "error", "running", "degraded", "ok"} {
		t.Run(scenario, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			state, _ := s.State(ctx)
			state.Phase = control.PhaseStable
			state.ActiveWorkload = control.WorkloadIdle
			state, err := s.UpdateState(ctx, state.Version, state)
			if err != nil {
				t.Fatal(err)
			}
			expected := control.Precondition{Incarnation: state.LeaseFence.Incarnation, Version: state.Version}
			want := error(nil)
			switch scenario {
			case "incarnation":
				expected.Incarnation = "other"
				want = ErrVersionConflict
			case "version":
				expected.Version--
				want = ErrVersionConflict
			case "user":
				state.Owner = control.OwnerUser
				want = ErrUserOwned
			case "phase":
				state.Phase = control.PhaseReconciling
				want = ErrRecoveryRequired
			case "error":
				state.Health = control.HealthError
				want = ErrRecoveryRequired
			case "degraded":
				state.Health = control.HealthDegraded
			case "running":
				if err := s.BeginTransition(ctx, Transition{ID: "existing", Fence: state.LeaseFence, Source: state, Target: state, Previous: state, Phase: state.Phase, Deadline: time.Now().Add(time.Minute)}); err != nil {
					t.Fatal(err)
				}
				want = ErrTransitionRunning
			}
			if scenario == "user" || scenario == "phase" || scenario == "error" || scenario == "degraded" {
				state, err = s.UpdateState(ctx, state.Version, state)
				if err != nil {
					t.Fatal(err)
				}
				expected.Version = state.Version
			}
			next, err := s.StartTransitionConditional(ctx, expected, Transition{ID: "conditional", Target: state, Previous: state, Deadline: time.Now().Add(time.Minute)})
			if !errors.Is(err, want) {
				t.Fatalf("error = %v, want %v", err, want)
			}
			if want == nil {
				if next.Version != state.Version+1 || next.Admission != control.AdmissionClosed {
					t.Fatalf("not fenced: %#v", next)
				}
				return
			}
			after, err := s.State(ctx)
			if err != nil || after != state {
				t.Fatalf("rejection changed state: %#v %v", after, err)
			}
			var count int
			if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM transitions WHERE transition_id = 'conditional'").Scan(&count); err != nil || count != 0 {
				t.Fatalf("inserted rejected transition: %d %v", count, err)
			}
		})
	}
}
