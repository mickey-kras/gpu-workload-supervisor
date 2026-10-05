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
	catalog := control.Catalog{Version: 1, Profiles: []control.Profile{{ID: "text", Label: "Text", Adapter: "systemd", Unit: "text.service", Cgroup: "/user/text", HealthURL: "http://localhost:9000", BootPolicy: "retain"}}}
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
	catalog := control.Catalog{Version: 1, Profiles: []control.Profile{{ID: "text", Label: "Text", Adapter: "systemd", Unit: "text.service", Cgroup: "/user/text", HealthURL: "http://localhost:9000"}}}
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
	catalog := control.Catalog{Version: 1, Profiles: []control.Profile{{ID: "text", Label: "Text", Adapter: "systemd", Unit: "text.service", Cgroup: "/user/text", HealthURL: "http://localhost:9000"}}}
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
