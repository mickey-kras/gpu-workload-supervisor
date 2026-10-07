package supervisor

import (
	"context"
	"errors"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

func TestActivateWorkloadStartsFromIdleAndRotatesFence(t *testing.T) {
	s := openStore(t)
	state := ownershipState(t, s, control.OwnerSupervisor, control.WorkloadIdle)
	r := &fakeRuntime{active: control.WorkloadIdle}
	c := testController(t, s, r)
	snap, err := s.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	expected := control.OperatorPrecondition{Incarnation: state.LeaseFence.Incarnation, Version: state.Version, Owner: state.Owner, ConfigurationRevision: snap.Revision}
	next, err := c.ActivateWorkload(context.Background(), control.WorkloadText, expected)
	if err != nil {
		t.Fatal(err)
	}
	if next.Owner != control.OwnerSupervisor || next.ActiveWorkload != control.WorkloadText || next.Admission != control.AdmissionOpen {
		t.Fatalf("activated state = %+v", next)
	}
	if next.LeaseFence == state.LeaseFence {
		t.Fatal("activation did not rotate the lease fence")
	}
	if len(r.calls) == 0 {
		t.Fatal("activation never started the workload")
	}
	// A replayed precondition is stale: exactly one execution generation.
	if _, err := c.ActivateWorkload(context.Background(), control.WorkloadText, expected); err == nil {
		t.Fatal("replay accepted")
	}
}

func TestActivateWorkloadDeferredWithoutLatchingOrRuntimeEffects(t *testing.T) {
	for _, mode := range []string{"active workload", "unfinished work"} {
		t.Run(mode, func(t *testing.T) {
			s := openStore(t)
			before := ownershipState(t, s, control.OwnerSupervisor, control.WorkloadText)
			if mode == "unfinished work" {
				if _, err := s.AdmitWorkToken(context.Background(), "req", "job", control.WorkloadText, before.LeaseFence); err != nil {
					t.Fatal(err)
				}
				// Drain to idle while the admitted work stays unfinished.
				drained := ownershipState(t, s, control.OwnerSupervisor, control.WorkloadIdle)
				before = drained
			}
			r := &fakeRuntime{active: control.WorkloadIdle}
			c := testController(t, s, r)
			snap, _ := s.Catalog(context.Background())
			expected := control.OperatorPrecondition{Incarnation: before.LeaseFence.Incarnation, Version: before.Version, Owner: before.Owner, ConfigurationRevision: snap.Revision}
			state, err := c.ActivateWorkload(context.Background(), control.WorkloadText, expected)
			if mode == "active workload" && !errors.Is(err, ErrNotIdle) {
				t.Fatalf("err = %v", err)
			}
			if mode == "unfinished work" && !errors.Is(err, ErrUnresolvedWork) {
				t.Fatalf("err = %v", err)
			}
			if state.Version != before.Version {
				t.Fatal("deferred activation changed state")
			}
			if len(r.calls) != 0 {
				t.Fatal("deferred activation drove runtime effects", r.calls)
			}
		})
	}
}

func TestActivateWorkloadRejectsWrongOwnerUnknownAndIdleTargets(t *testing.T) {
	s := openStore(t)
	state := ownershipState(t, s, control.OwnerSupervisor, control.WorkloadIdle)
	c := testController(t, s, &fakeRuntime{active: control.WorkloadIdle})
	snap, _ := s.Catalog(context.Background())
	expected := control.OperatorPrecondition{Incarnation: state.LeaseFence.Incarnation, Version: state.Version, Owner: state.Owner, ConfigurationRevision: snap.Revision}
	if _, err := c.ActivateWorkload(context.Background(), control.WorkloadIdle, expected); err == nil {
		t.Fatal("idle target accepted")
	}
	if _, err := c.ActivateWorkload(context.Background(), "unknown-target", expected); err == nil {
		t.Fatal("unconfigured target accepted")
	}
	user := expected
	user.Owner = control.OwnerUser
	if _, err := c.ActivateWorkload(context.Background(), control.WorkloadText, user); !errors.Is(err, store.ErrWrongOwner) {
		t.Fatalf("user ownership: %v", err)
	}
}
