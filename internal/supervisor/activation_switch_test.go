package supervisor

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

func activationExpected(t *testing.T, s *store.Store, state control.State) control.OperatorPrecondition {
	t.Helper()
	snap, err := s.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return control.OperatorPrecondition{Incarnation: state.LeaseFence.Incarnation, Version: state.Version, Owner: state.Owner, ConfigurationRevision: snap.Revision}
}

func TestActivateWorkloadSwitchesFromStableWorkloadWithoutWork(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	before := ownershipState(t, s, control.OwnerSupervisor, control.WorkloadText)
	r := &fakeRuntime{active: control.WorkloadText}
	c := testController(t, s, r)
	next, err := c.ActivateWorkload(ctx, control.WorkloadMedia, activationExpected(t, s, before))
	if err != nil {
		t.Fatal(err)
	}
	if next.ActiveWorkload != control.WorkloadMedia || next.Admission != control.AdmissionOpen || next.LeaseFence == before.LeaseFence {
		t.Fatalf("switched state = %+v", next)
	}
	stop, start := slices.Index(r.calls, "stop text"), slices.Index(r.calls, "start media")
	if stop < 0 || start < stop {
		t.Fatalf("switch did not stop text before starting media: %v", r.calls)
	}
}

func TestActivateWorkloadSwitchWaitsForInFlightWork(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	before := ownershipState(t, s, control.OwnerSupervisor, control.WorkloadText)
	token, err := s.AdmitWorkToken(ctx, "in-flight", "job", control.WorkloadText, before.LeaseFence)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r := &fakeRuntime{active: control.WorkloadText}
	c := testController(t, s, r)
	state, err := c.ActivateWorkload(ctx, control.WorkloadMedia, activationExpected(t, s, admitted))
	if !errors.Is(err, ErrUnresolvedWork) {
		t.Fatalf("switch with in-flight work: %v", err)
	}
	if state.Version != admitted.Version || state.Admission != control.AdmissionOpen || len(r.calls) != 0 {
		t.Fatalf("deferred switch changed state or runtime: %+v %v", state, r.calls)
	}
	if err := s.FinishWorkToken(ctx, "in-flight", control.WorkloadText, admitted.LeaseFence, token, store.WorkCompleted); err != nil {
		t.Fatal(err)
	}
	finished, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	next, err := c.ActivateWorkload(ctx, control.WorkloadMedia, activationExpected(t, s, finished))
	if err != nil {
		t.Fatal(err)
	}
	if next.ActiveWorkload != control.WorkloadMedia {
		t.Fatalf("switch after work finished = %+v", next)
	}
}

func TestActivateWorkloadNeverSwitchesAwayFromTrainer(t *testing.T) {
	ctx := context.Background()
	fixture, _, _ := trainingStartContract(t)
	catalog := fixture.config.Catalog.Catalog.Clone()
	catalog.Profiles = append(catalog.Profiles, testCatalog().Profiles...)
	s := openStoreAt(t, filepath.Join(t.TempDir(), "state.db"))
	if _, err := s.ReplaceCatalog(ctx, "", catalog); err != nil {
		t.Fatal(err)
	}
	before := ownershipState(t, s, control.OwnerSupervisor, "trainer")
	r := &activationRuntime{fakeRuntime: fakeRuntime{active: "trainer"}}
	c := testController(t, s, r)
	if _, err := c.ActivateWorkload(ctx, control.WorkloadText, activationExpected(t, s, before)); !errors.Is(err, ErrNotIdle) {
		t.Fatalf("switch away from trainer: %v", err)
	}
	after, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) || len(r.calls) != 0 {
		t.Fatalf("deferred trainer switch changed state or runtime: %+v %v", after, r.calls)
	}
}
