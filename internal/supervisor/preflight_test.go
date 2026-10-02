package supervisor

import (
	"context"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"testing"
)

type preflightRuntime struct {
	*fakeRuntime
	err error
}

func (r preflightRuntime) Preflight(context.Context) error { return r.err }
func TestPreflightRejectsBeforeTransitionEffects(t *testing.T) {
	for _, owner := range []control.Owner{control.OwnerSupervisor, control.OwnerUser} {
		t.Run(string(owner), func(t *testing.T) {
			s := openStore(t)
			before := ownershipState(t, s, owner, control.WorkloadText)
			r := &fakeRuntime{active: control.WorkloadText}
			cause := errors.New("unsupported host")
			c := testController(t, s, preflightRuntime{r, cause})
			_, err := c.transition(context.Background(), control.WorkloadMedia, owner, control.OwnerSupervisor, "test", false)
			if !errors.Is(err, cause) {
				t.Fatalf("preflight error %v", err)
			}
			after, _ := s.State(context.Background())
			if after.Version != before.Version || len(r.calls) != 0 {
				t.Fatalf("preflight caused mutation: %#v %v", after, r.calls)
			}
		})
	}
}

func TestRecoveryPreflightPreservesClosedEntryWithoutRuntimeEffects(t *testing.T) {
	for _, resolve := range []bool{false, true} {
		s := openStore(t)
		before := ownershipState(t, s, control.OwnerSupervisor, control.WorkloadText)
		r := &fakeRuntime{active: control.WorkloadText}
		cause := errors.New("unsupported host")
		c := testController(t, s, preflightRuntime{r, cause})
		var err error
		if resolve {
			_, _, err = c.ResolveUnfinishedWork(context.Background(), "test")
		} else {
			_, err = c.Recover(context.Background())
		}
		if !errors.Is(err, cause) {
			t.Fatalf("preflight: %v", err)
		}
		after, _ := s.State(context.Background())
		if after.Admission != control.AdmissionClosed || after.LeaseFence == before.LeaseFence || len(r.calls) != 0 {
			t.Fatalf("recovery entry %#v calls %v", after, r.calls)
		}
	}
}

func TestReconcilePreflightRejectsBeforeStop(t *testing.T) {
	s := openStore(t)
	r := &fakeRuntime{active: control.WorkloadMedia}
	cause := errors.New("unsupported host")
	c := testController(t, s, preflightRuntime{r, cause})
	state, err := c.Reconcile(context.Background())
	if !errors.Is(err, cause) || state.Admission != control.AdmissionClosed || len(r.calls) != 0 {
		t.Fatalf("reconcile preflight %#v %v calls %v", state, err, r.calls)
	}
}
