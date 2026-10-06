package main

import (
	"context"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
	"os"
	"path/filepath"
	"testing"
)

func TestGenericSwitchTarget(t *testing.T) {
	if err := validateTarget("switch", "speech"); err != nil {
		t.Fatal(err)
	}
	if control.ValidWorkloadID("unknown") {
		t.Fatal("unknown selectable")
	}
}

type cliCatalogRuntime struct{ active control.Workload }

func (r *cliCatalogRuntime) Observe(context.Context) (gpuruntime.Snapshot, error) {
	return gpuruntime.Snapshot{Workloads: map[control.Workload]gpuruntime.WorkloadObservation{"speech": {Active: r.active == "speech", Exclusive: true}}}, nil
}
func (r *cliCatalogRuntime) Start(_ context.Context, id control.Workload) error {
	r.active = id
	return nil
}
func (r *cliCatalogRuntime) Stop(_ context.Context, id control.Workload) error {
	if r.active == id {
		r.active = control.WorkloadIdle
	}
	return nil
}
func (r *cliCatalogRuntime) StopForRecovery(context.Context) error {
	r.active = control.WorkloadIdle
	return nil
}
func (r *cliCatalogRuntime) Healthy(context.Context, control.Workload) error { return nil }
func (r *cliCatalogRuntime) ReleasedFor(context.Context, control.Workload) error {
	return nil
}
func (r *cliCatalogRuntime) Preflight(context.Context) error { return nil }
func TestCLIThirdCatalogWorkloadAndFailedReload(t *testing.T) {
	oldArgs, oldStdout := os.Args, os.Stdout
	t.Cleanup(func() { os.Args, os.Stdout = oldArgs, oldStdout })
	out, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	os.Stdout = out
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.db")
	file := filepath.Join(dir, "catalog.json")
	raw := `{"version":1,"profiles":[{"id":"speech","label":"Speech","adapter":"systemd","unit":"speech.service","cgroup":"/workloads/speech.service","healthURL":"http://localhost:9000"}]}`
	if err = os.WriteFile(file, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	r := &cliCatalogRuntime{}
	factory := func(cfg gpuruntime.SystemdConfig) (gpuruntime.Manager, error) {
		if cfg.Catalog == nil {
			t.Fatal("catalog not pinned")
		}
		return r, nil
	}
	invoke := func(args ...string) error {
		os.Args = append([]string{"gpu-mode", "-state", statePath}, args...)
		return runWithRuntimeFactory(factory)
	}
	if err = invoke("-catalog", file, "configure"); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"reconcile"}, {"-workload", "speech", "switch"}, {"status"}} {
		if err = invoke(args...); err != nil {
			t.Fatal(err)
		}
	}
	if err = invoke("-workload", "unconfigured", "switch"); err == nil {
		t.Fatal("unconfigured target accepted")
	}
	if err = os.WriteFile(file, []byte(`{broken`), 0600); err != nil {
		t.Fatal(err)
	}
	if err = invoke("-catalog", file, "configure"); err == nil {
		t.Fatal("invalid reload accepted")
	}
	if err = invoke("status"); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(context.Background(), statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	state, err := s.State(context.Background())
	if err != nil || state.ActiveWorkload != "speech" {
		t.Fatalf("%+v %v", state, err)
	}
	token, err := s.AdmitWorkToken(context.Background(), "request", "", "speech", state.LeaseFence)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.FinishWorkToken(context.Background(), "request", "speech", state.LeaseFence, token, store.WorkCompleted); err != nil {
		t.Fatal(err)
	}
}
