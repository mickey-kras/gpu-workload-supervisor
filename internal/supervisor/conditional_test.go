package supervisor

import (
	"context"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
	"testing"
)

type concurrentConditionalStore struct {
	*store.Store
	mutate func(context.Context)
}

func (s concurrentConditionalStore) StartTransitionConditional(ctx context.Context, p control.Precondition, tr store.Transition) (control.State, error) {
	s.mutate(ctx)
	return s.Store.StartTransitionConditional(ctx, p, tr)
}

func TestConditionalSwitchCannotLoseClientPrecondition(t *testing.T) {
	for _, scenario := range []string{"incarnation", "version", "concurrent-version", "concurrent-owner", "ok"} {
		t.Run(scenario, func(t *testing.T) {
			s := openStore(t)
			runtime := &fakeRuntime{active: control.WorkloadText}
			c := testController(t, s, runtime)
			state, err := c.Reconcile(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			runtime.calls = nil
			expected := control.Precondition{Incarnation: state.LeaseFence.Incarnation, Version: state.Version}
			if scenario == "incarnation" {
				expected.Incarnation = "other"
			}
			if scenario == "version" {
				expected.Version--
			}
			if scenario == "concurrent-version" || scenario == "concurrent-owner" {
				c.store = concurrentConditionalStore{s, func(ctx context.Context) {
					next := state
					if scenario == "concurrent-owner" {
						next.Owner = control.OwnerUser
						next.Admission = control.AdmissionClosed
					}
					if _, err := s.UpdateState(ctx, state.Version, next); err != nil {
						t.Fatal(err)
					}
				}}
			}
			got, err := c.SwitchConditional(context.Background(), control.WorkloadMedia, "opaque", expected)
			if scenario == "ok" {
				if err != nil || got.ActiveWorkload != control.WorkloadMedia {
					t.Fatalf("switch: %#v %v", got, err)
				}
				return
			}
			if !errors.Is(err, store.ErrVersionConflict) {
				t.Fatalf("lost precondition: %v", err)
			}
			if len(runtime.calls) != 0 {
				t.Fatalf("runtime changed: %v", runtime.calls)
			}
			if running, err := s.InProgressTransition(context.Background()); err != nil || running != "" {
				t.Fatalf("transition inserted: %q %v", running, err)
			}
		})
	}
}
