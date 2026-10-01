package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

// The runtime package covers kernel evidence and blank ControlGroup metadata.
// This boundary fixture tests fresh CLI configuration, state and recovery wiring
// without requiring a real user systemd manager or exposing a filesystem bypass.
type stoppedRecoveryRuntime struct {
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
func (r *stoppedRecoveryRuntime) Released(context.Context) error {
	r.releaseCalls++
	return nil
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
		os.Args = []string{"gpu-mode", "-state", statePath, "-text-unit", "text.service", "-media-unit", "media.service", "-text-cgroup", "/workloads/text.service", "-media-cgroup", "/workloads/media.service", "-text-health-url", "http://127.0.0.1:1/", "-media-health-url", "http://127.0.0.1:1/", "-media-stop-mode", "stop-service", "-systemctl", "/usr/bin/true", "-target", "idle", "recover-user"}
		runtime := &stoppedRecoveryRuntime{}
		factory := func(config gpuruntime.SystemdConfig) (gpuruntime.Manager, error) {
			if config.TextCgroup != "/workloads/text.service" || config.MediaCgroup != "/workloads/media.service" || config.MediaStopMode != gpuruntime.MediaStopService {
				t.Fatalf("CLI dropped explicit identity: %#v", config)
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
