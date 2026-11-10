package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/supervisor"
)

// The runtime package covers kernel evidence and blank ControlGroup metadata.
// This boundary fixture tests fresh CLI configuration, state and recovery wiring
// without requiring a real user systemd manager or exposing a filesystem bypass.
type stoppedRecoveryRuntime struct {
	noLimitsOrTraining
	releaseCalls int
}

func (r *stoppedRecoveryRuntime) Observe(context.Context) (gpuruntime.Snapshot, error) {
	return gpuruntime.Snapshot{}, nil
}
func (r *stoppedRecoveryRuntime) Start(context.Context, control.Workload) error {
	return fmt.Errorf("verify-only recovery unexpectedly started a unit")
}
func (r *stoppedRecoveryRuntime) Stop(context.Context, control.Workload) error {
	return fmt.Errorf("verify-only recovery unexpectedly stopped a unit")
}
func (r *stoppedRecoveryRuntime) StopForRecovery(context.Context) error {
	return fmt.Errorf("verify-only recovery unexpectedly stopped runtimes")
}
func (r *stoppedRecoveryRuntime) Healthy(context.Context, control.Workload) error {
	return nil
}
func (r *stoppedRecoveryRuntime) ReleasedFor(context.Context, control.Workload) error {
	r.releaseCalls++
	return nil
}
func (r *stoppedRecoveryRuntime) Preflight(context.Context) error { return nil }
func (r *stoppedRecoveryRuntime) ObserveForRecovery(ctx context.Context) (gpuruntime.Snapshot, error) {
	return r.Observe(ctx)
}

// noLimitsOrTraining is the part of a fake runtime that cannot check
// limits or run trainers.
type noLimitsOrTraining struct{}

var errFakeTraining = errors.New("the fake runs no trainer")

func (noLimitsOrTraining) VerifyLaunchLimits(context.Context, control.Workload) error {
	return gpuruntime.ErrLimitUnchecked
}
func (noLimitsOrTraining) VerifyLimitProtection(context.Context, control.Workload) error {
	return gpuruntime.ErrLimitUnchecked
}
func (noLimitsOrTraining) LimitOutcome(context.Context, control.Workload) gpuruntime.LimitOutcome {
	return gpuruntime.LimitOutcome{Class: gpuruntime.LimitOutcomeNone}
}
func (noLimitsOrTraining) LimitStopReleased(context.Context, control.Workload) error {
	return gpuruntime.ErrLimitUnchecked
}
func (noLimitsOrTraining) StartTraining(context.Context, control.WorkloadProfile, control.TrainingJob) (string, error) {
	return "", errFakeTraining
}
func (noLimitsOrTraining) TrainingTerminal(context.Context, control.WorkloadProfile, control.TrainingJob) (control.TrainingTerminal, error) {
	return control.TrainingTerminal{}, errFakeTraining
}
func (noLimitsOrTraining) ReleaseTraining(context.Context, control.WorkloadProfile) error {
	return errFakeTraining
}
func (noLimitsOrTraining) TrainingMemoryPeak(context.Context, control.WorkloadProfile, control.TrainingJob) (uint64, error) {
	return 0, errFakeTraining
}

func TestFreshCLIUserIdleRecoveryWiresConfiguredCgroups(t *testing.T) {
	ctx := context.Background()
	statePath := filepath.Join(t.TempDir(), "state.db")
	stateStore, err := store.Open(ctx, statePath)
	if err != nil {
		t.Fatal(err)
	}
	before, err := stateStore.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	before.Owner = control.OwnerUser
	before.Health = control.HealthError
	before.Phase = control.PhaseReconciling
	before.ActiveWorkload = control.WorkloadUnknown
	before, err = stateStore.UpdateState(ctx, before.Version, before)
	if err != nil {
		t.Fatal(err)
	}
	catalog := control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{
		{ID: "text", Label: "Text", Adapter: "systemd", Unit: "text.service", Cgroup: "/workloads/text.service", HealthURL: "http://127.0.0.1:1/"},
		{ID: "media", Label: "Media", Adapter: "systemd", Unit: "media.service", Cgroup: "/workloads/media.service", HealthURL: "http://127.0.0.1:1/"},
	}}
	if _, err := stateStore.ReplaceCatalog(ctx, "", catalog); err != nil {
		t.Fatal(err)
	}
	if err := stateStore.Close(); err != nil {
		t.Fatal(err)
	}
	oldArgs, oldStdout := os.Args, os.Stdout
	t.Cleanup(func() { os.Args, os.Stdout = oldArgs, oldStdout })
	for invocation := 0; invocation < 2; invocation++ {
		output, err := os.CreateTemp(t.TempDir(), "output")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { output.Close() })
		os.Stdout = output
		os.Args = []string{"gpu-mode", "-state", statePath, "-systemctl", "/usr/bin/true", "-target", "idle", "recover-user"}
		runtime := &stoppedRecoveryRuntime{}
		factory := func(config gpuruntime.SystemdConfig) (supervisor.Runtime, error) {
			if config.Catalog == nil {
				t.Fatalf("accepted catalog not pinned: %#v", config)
			}
			text, _ := config.Catalog.Profile("text")
			media, _ := config.Catalog.Profile("media")
			if text.Cgroup != "/workloads/text.service" || media.Cgroup != "/workloads/media.service" {
				t.Fatalf("CLI dropped accepted identity: %#v", config.Catalog)
			}
			return runtime, nil
		}
		if err := runWithRuntimeFactory(factory); err != nil {
			t.Fatal(err)
		}
		if _, err := output.Seek(0, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		var state control.State
		if err := json.NewDecoder(output).Decode(&state); err != nil {
			t.Fatal(err)
		}
		if state.Owner != control.OwnerUser || state.ActiveWorkload != control.WorkloadIdle || state.Admission != control.AdmissionClosed || state.Health != control.HealthHealthy || state.LeaseFence == before.LeaseFence || runtime.releaseCalls == 0 {
			t.Fatalf("fresh invocation %d: %#v release calls %d", invocation, state, runtime.releaseCalls)
		}
		before = state
	}
}
