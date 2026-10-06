package supervisor

import (
	"context"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
	"testing"
)

type catalogRuntime struct{ fakeRuntime }

func (r *catalogRuntime) Observe(context.Context) (gpuruntime.Snapshot, error) {
	return gpuruntime.Snapshot{Workloads: map[control.Workload]gpuruntime.WorkloadObservation{"speech": {Active: r.active == "speech", Exclusive: true}}}, nil
}
func (r *catalogRuntime) Start(_ context.Context, id control.Workload) error {
	r.active = id
	return nil
}
func TestThirdWorkloadLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	r := &catalogRuntime{}
	c := testController(t, s, r)
	catalog := control.Catalog{Version: 1, Profiles: []control.Profile{{ID: "speech", Label: "Speech", Adapter: "systemd", Unit: "speech.service", Cgroup: "/user/speech", HealthURL: "http://localhost:9000"}}}
	snap, err := s.ReplaceCatalog(ctx, c.config.Catalog.Revision, catalog)
	if err != nil {
		t.Fatal(err)
	}
	c.config.Catalog = &snap
	if _, err = c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	state, err := c.Switch(ctx, "speech", "test")
	if err != nil {
		t.Fatal(err)
	}
	if state.ActiveWorkload != "speech" {
		t.Fatal(state)
	}
	token, err := s.AdmitWorkToken(ctx, "speech-job", "", "speech", state.LeaseFence)
	if err != nil {
		t.Fatal(err)
	}
	if token == "" {
		t.Fatal("missing token")
	}
}
func TestStatusRejectsChangedCatalogBeforeObservation(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	r := &catalogRuntime{}
	c := testController(t, s, r)
	catalog := control.Catalog{Version: 1, Profiles: []control.Profile{{ID: "speech", Label: "Speech", Adapter: "systemd", Unit: "speech.service", Cgroup: "/user/speech", HealthURL: "http://localhost:9000"}}}
	if _, err := s.ReplaceCatalog(ctx, c.config.Catalog.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	before, _ := s.State(ctx)
	if _, err := c.Status(ctx); err == nil {
		t.Fatal("old controller observed new catalog")
	}
	after, _ := s.State(ctx)
	if before != after {
		t.Fatal("old controller mutated state")
	}
}

func TestCatalogSnapshotExclusivity(t *testing.T) {
	s := gpuruntime.Snapshot{Workloads: map[control.Workload]gpuruntime.WorkloadObservation{"speech": {Active: true, Exclusive: true}, "media": {Active: true, Exclusive: true}}}
	if err := verifySnapshot("speech", s); !errors.Is(err, ErrStateVerification) {
		t.Fatal(err)
	}
	state := control.State{ActiveWorkload: "speech"}
	if _, err := observedWorkload(state, s); !errors.Is(err, ErrInvariant) {
		t.Fatal(err)
	}
	delete(s.Workloads, "speech")
	if err := verifySnapshot("speech", s); err == nil {
		t.Fatal("missing target accepted")
	}
	s.Workloads["media"] = gpuruntime.WorkloadObservation{}
	if err := verifySnapshot("speech", s); err == nil {
		t.Fatal("inactive target accepted")
	}
}

type failedCatalogPreflight struct{ catalogRuntime }

func (r *failedCatalogPreflight) Preflight(context.Context) error {
	return errors.New("cgroup unavailable")
}
func TestCatalogReconcilePreflightFailureClosesAdmission(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	catalog := control.Catalog{Version: 1, Profiles: []control.Profile{{ID: "speech", Label: "Speech", Adapter: "systemd", Unit: "speech.service", Cgroup: "/user/speech", HealthURL: "http://localhost:9000"}}}
	snap, err := s.ReplaceCatalog(ctx, "", catalog)
	if err != nil {
		t.Fatal(err)
	}
	state, _ := s.State(ctx)
	state = stableTarget(state, control.OwnerSupervisor, "speech")
	if _, err = s.UpdateState(ctx, state.Version, state); err != nil {
		t.Fatal(err)
	}
	c := testController(t, s, &failedCatalogPreflight{})
	c.config.Catalog = &snap
	if _, err = c.Reconcile(ctx); err == nil {
		t.Fatal("failure hidden")
	}
	state, _ = s.State(ctx)
	if state.Admission != control.AdmissionClosed || state.Health != control.HealthError {
		t.Fatal("admission not latched", state)
	}
}
func TestCatalogReconcileRetainsAdmittedWorkWithoutFenceRotation(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	catalog := control.Catalog{Version: 1, Profiles: []control.Profile{{ID: "speech", Label: "Speech", Adapter: "systemd", Unit: "speech.service", Cgroup: "/user/speech", HealthURL: "http://localhost:9000", BootPolicy: "retain"}}}
	snap, err := s.ReplaceCatalog(ctx, "", catalog)
	if err != nil {
		t.Fatal(err)
	}
	state, _ := s.State(ctx)
	state = stableTarget(state, control.OwnerSupervisor, "speech")
	state, err = s.UpdateState(ctx, state.Version, state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdmitWorkToken(ctx, "ongoing", "", "speech", state.LeaseFence); err != nil {
		t.Fatal(err)
	}
	r := &catalogRuntime{}
	r.active = "speech"
	c := testController(t, s, r)
	c.config.Catalog = &snap
	after, err := c.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.LeaseFence != state.LeaseFence || after.Version != state.Version {
		t.Fatal("healthy retained allocation fenced", after)
	}
}
func TestCatalogRecoveryRetainsWorkCompletionAuthority(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	catalog := control.Catalog{Version: 1, Profiles: []control.Profile{{ID: "speech", Label: "Speech", Adapter: "systemd", Unit: "speech.service", Cgroup: "/user/speech", HealthURL: "http://localhost:9000", BootPolicy: "retain"}}}
	snap, err := s.ReplaceCatalog(ctx, "", catalog)
	if err != nil {
		t.Fatal(err)
	}
	state, _ := s.State(ctx)
	state = stableTarget(state, control.OwnerSupervisor, "speech")
	state, err = s.UpdateState(ctx, state.Version, state)
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.AdmitWorkToken(ctx, "ongoing", "", "speech", state.LeaseFence)
	if err != nil {
		t.Fatal(err)
	}
	r := &catalogRuntime{}
	r.active = "speech"
	c := testController(t, s, r)
	c.config.Catalog = &snap
	after, err := c.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.ActiveWorkload != "speech" || after.Admission != control.AdmissionOpen {
		t.Fatal(after)
	}
	if err = s.FinishWorkToken(ctx, "ongoing", "speech", state.LeaseFence, token, store.WorkCompleted); err != nil {
		t.Fatal(err)
	}
}
