package supervisor

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
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

type activationRuntime struct{ fakeRuntime }

func (r *activationRuntime) Preflight(context.Context) error {
	r.calls = append(r.calls, "preflight")
	return nil
}

func (r *activationRuntime) Observe(ctx context.Context) (gpuruntime.Snapshot, error) {
	r.calls = append(r.calls, "observe")
	snapshot, err := r.fakeRuntime.Observe(ctx)
	if err == nil {
		snapshot.Workloads["trainer"] = gpuruntime.WorkloadObservation{Active: r.active == "trainer", Exclusive: true}
	}
	return snapshot, err
}

func (r *activationRuntime) Healthy(ctx context.Context, workload control.Workload) error {
	r.calls = append(r.calls, "healthy "+string(workload))
	return r.fakeRuntime.Healthy(ctx, workload)
}

func TestActivateWorkloadRejectsTrainerBeforeAnyEffects(t *testing.T) {
	ctx := context.Background()
	fixture, _, _ := trainingStartContract(t)
	catalog := fixture.config.Catalog.Catalog.Clone()
	catalog.Profiles = append(catalog.Profiles, testCatalog().Profiles...)
	for _, active := range []control.Workload{control.WorkloadIdle, "trainer"} {
		t.Run(string(active), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			s := openStoreAt(t, path)
			snap, err := s.ReplaceCatalog(ctx, "", catalog)
			if err != nil {
				t.Fatal(err)
			}
			before := ownershipState(t, s, control.OwnerSupervisor, control.WorkloadIdle)
			r := &activationRuntime{fakeRuntime: fakeRuntime{active: active}}
			c := testController(t, s, r)
			expected := control.OperatorPrecondition{Incarnation: before.LeaseFence.Incarnation, Version: before.Version, Owner: before.Owner, ConfigurationRevision: snap.Revision}
			if _, err := c.ActivateWorkload(ctx, "trainer", expected); err == nil || err.Error() != "finite trainer requires training-start" {
				t.Fatalf("trainer activation error = %v", err)
			}
			after, err := s.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("rejected trainer changed state: before=%+v after=%+v", before, after)
			}
			if len(r.calls) != 0 || r.releaseCalls != 0 {
				t.Fatalf("rejected trainer called runtime: calls=%v release=%d", r.calls, r.releaseCalls)
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			for _, table := range []string{"transitions", "transition_events", "transition_work", "registered_work", "training_jobs", "training_claims"} {
				var count int
				if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count != 0 {
					t.Fatalf("rejected trainer wrote %s: %d rows", table, count)
				}
			}
			if active != control.WorkloadIdle {
				return
			}
			next, err := c.ActivateWorkload(ctx, control.WorkloadText, expected)
			if err != nil {
				t.Fatal(err)
			}
			if next.ActiveWorkload != control.WorkloadText || next.Admission != control.AdmissionOpen || next.LeaseFence == before.LeaseFence {
				t.Fatalf("inference activation state = %+v", next)
			}
		})
	}
}
