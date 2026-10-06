package supervisor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

func TestOperatorPreservesTakeoverAndReturnsIdle(t *testing.T) {
	s := openStore(t)
	catalog := control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "text", Label: "Text", Adapter: "systemd", Unit: "text.service", Cgroup: "/user/text", HealthURL: "http://localhost:9000", BootPolicy: "retain"}}}
	snap, e := s.ReplaceCatalog(context.Background(), "", catalog)
	if e != nil {
		t.Fatal(e)
	}
	state := ownershipState(t, s, control.OwnerSupervisor, control.WorkloadText)
	r := &fakeRuntime{active: control.WorkloadText}
	c := testController(t, s, r)
	c.config.Catalog = &snap
	expected := control.OperatorPrecondition{Incarnation: state.LeaseFence.Incarnation, Version: state.Version, Owner: state.Owner, ConfigurationRevision: snap.Revision}
	next, e := c.OperatorTransition(context.Background(), "take-control", "", expected)
	if e != nil || next.Owner != control.OwnerUser || next.ActiveWorkload != control.WorkloadText {
		t.Fatal(next, e)
	}
	if len(r.calls) != 0 {
		t.Fatal("takeover restarted workload", r.calls)
	}
	if _, e = c.OperatorTransition(context.Background(), "take-control", "", expected); !errors.Is(e, store.ErrVersionConflict) {
		t.Fatal("duplicate accepted", e)
	}
	c.id = func() (string, error) { return "return", nil }
	expected.Version = next.Version
	expected.Owner = next.Owner
	next, e = c.OperatorTransition(context.Background(), "return-control", "", expected)
	if e != nil || next.Owner != control.OwnerSupervisor || next.ActiveWorkload != control.WorkloadIdle {
		t.Fatal(next, e)
	}
}

type racingOperatorStore struct {
	*store.Store
	mutate func(context.Context)
}

func (s racingOperatorStore) StartOperatorTransition(ctx context.Context, e control.OperatorPrecondition, tr store.Transition) (control.State, error) {
	s.mutate(ctx)
	return s.Store.StartOperatorTransition(ctx, e, tr)
}
func TestOperatorRaceCannotReachRuntimeEffects(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	catalog := control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "text", Label: "Text", Adapter: "systemd", Unit: "text.service", Cgroup: "/user/text", HealthURL: "http://localhost:9000"}}}
	snap, e := s.ReplaceCatalog(ctx, "", catalog)
	if e != nil {
		t.Fatal(e)
	}
	state := ownershipState(t, s, control.OwnerUser, control.WorkloadText)
	r := &fakeRuntime{active: control.WorkloadText}
	c := testController(t, s, r)
	c.config.Catalog = &snap
	c.store = racingOperatorStore{s, func(ctx context.Context) {
		next := state
		next.Owner = control.OwnerSupervisor
		if _, e := s.UpdateState(ctx, state.Version, next); e != nil {
			t.Fatal(e)
		}
	}}
	_, e = c.OperatorTransition(ctx, "return-control", "", control.OperatorPrecondition{Incarnation: state.LeaseFence.Incarnation, Version: state.Version, Owner: state.Owner, ConfigurationRevision: snap.Revision})
	if !errors.Is(e, store.ErrVersionConflict) || len(r.calls) != 0 {
		t.Fatal(e, r.calls)
	}
}
func TestTakeoverDrainFailureNeverRestartsWorkload(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	catalog := control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "text", Label: "Text", Adapter: "systemd", Unit: "text.service", Cgroup: "/user/text", HealthURL: "http://localhost:9000"}}}
	snap, e := s.ReplaceCatalog(ctx, "", catalog)
	if e != nil {
		t.Fatal(e)
	}
	state := ownershipState(t, s, control.OwnerSupervisor, control.WorkloadText)
	if _, e = s.AdmitWorkToken(ctx, "unfinished", "", control.WorkloadText, state.LeaseFence); e != nil {
		t.Fatal(e)
	}
	r := &fakeRuntime{active: control.WorkloadText}
	c := testController(t, s, r)
	c.config.Catalog = &snap
	c.config.DrainTimeout = time.Millisecond
	_, e = c.OperatorTransition(ctx, "take-control", "", control.OperatorPrecondition{Incarnation: state.LeaseFence.Incarnation, Version: state.Version, Owner: state.Owner, ConfigurationRevision: snap.Revision})
	if !errors.Is(e, ErrDrainTimeout) {
		t.Fatal(e)
	}
	if len(r.calls) != 0 {
		t.Fatal("takeover failure disturbed existing work", r.calls)
	}
	after, e := s.State(ctx)
	if e != nil || after.Owner != control.OwnerSupervisor || after.Admission != control.AdmissionClosed || after.Health != control.HealthError {
		t.Fatal(after, e)
	}
}

func operatorFixture(t *testing.T, runtime *fakeRuntime) (*Controller, control.OperatorPrecondition) {
	t.Helper()
	s := openStore(t)
	snap := installTestCatalog(t, s)
	state := ownershipState(t, s, control.OwnerSupervisor, control.WorkloadText)
	c := testController(t, s, runtime)
	return c, control.OperatorPrecondition{Incarnation: state.LeaseFence.Incarnation, Version: state.Version, Owner: state.Owner, ConfigurationRevision: snap.Revision}
}

func operatorSideEffects(t *testing.T, c *Controller, runtime *fakeRuntime) {
	t.Helper()
	if len(runtime.calls) != 0 {
		t.Fatalf("rejected request reached runtime: %v", runtime.calls)
	}
	if id, err := c.store.InProgressTransition(context.Background()); err != nil || id != "" {
		t.Fatalf("rejected request started transition %q: %v", id, err)
	}
}

func TestOperatorRejectsUnknownActionBeforeAnyEffect(t *testing.T) {
	runtime := &fakeRuntime{active: control.WorkloadText}
	c, e := operatorFixture(t, runtime)
	if _, err := c.OperatorTransition(context.Background(), "restart", "", e); err == nil {
		t.Fatal("unknown action accepted")
	}
	operatorSideEffects(t, c, runtime)
}

func TestOperatorRejectsWrongSourceOwnerBeforeAnyEffect(t *testing.T) {
	runtime := &fakeRuntime{active: control.WorkloadText}
	c, e := operatorFixture(t, runtime)
	e.Owner = control.OwnerUser
	if _, err := c.OperatorTransition(context.Background(), "take-control", "", e); !errors.Is(err, store.ErrWrongOwner) {
		t.Fatalf("wrong owner = %v", err)
	}
	operatorSideEffects(t, c, runtime)
}

func TestOperatorTakeoverAbortsWhenStatusUnavailable(t *testing.T) {
	runtime := &fakeRuntime{active: control.WorkloadText, observeErr: errors.New("runtime obscured")}
	c, e := operatorFixture(t, runtime)
	if _, err := c.OperatorTransition(context.Background(), "take-control", "", e); err == nil {
		t.Fatal("takeover proceeded without runtime status")
	}
	operatorSideEffects(t, c, runtime)
}

func TestOperatorTakeoverAbortsWhenWorkloadNotReady(t *testing.T) {
	s := openStore(t)
	snap := installTestCatalog(t, s)
	state := ownershipState(t, s, control.OwnerSupervisor, control.WorkloadText)
	// A closed gate keeps Status observational, so the readiness gate below it
	// is the first check that fails.
	closed := state
	closed.Admission = control.AdmissionClosed
	closed, err := s.UpdateState(context.Background(), state.Version, closed)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &fakeRuntime{active: control.WorkloadText, healthFailures: 1}
	c := testController(t, s, runtime)
	e := control.OperatorPrecondition{Incarnation: closed.LeaseFence.Incarnation, Version: closed.Version, Owner: closed.Owner, ConfigurationRevision: snap.Revision}
	if _, err := c.OperatorTransition(context.Background(), "take-control", "", e); !errors.Is(err, ErrHealthCheck) {
		t.Fatalf("unready workload = %v", err)
	}
	operatorSideEffects(t, c, runtime)
}
