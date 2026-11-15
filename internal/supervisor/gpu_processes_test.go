package supervisor

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

var errGPUBusy = &gpuruntime.GPUInUseError{Device: "0", Processes: []gpuruntime.GPUProcess{{PID: 7, Name: "game", UID: 1000, User: "alice", Closable: true}}}
var errGPUCheck = &gpuruntime.GPUCheckError{Reason: "the nvidia-smi process query timed out"}

// A blocked start changes nothing: no stop, no start and no state write.
// The running workload keeps running.
func TestUserSwitchRefusedWhileOtherProgramUsesGPU(t *testing.T) {
	for name, cause := range map[string]error{"in use": errGPUBusy, "check failed": errGPUCheck} {
		t.Run(name, func(t *testing.T) {
			s := openStore(t)
			before := ownershipState(t, s, control.OwnerUser, control.WorkloadText)
			r := &fakeRuntime{active: control.WorkloadText, gpuErr: cause}
			c := testController(t, s, r)
			_, err := c.SwitchUser(context.Background(), control.WorkloadMedia, "operator")
			if !errors.Is(err, cause) || !errors.Is(err, ErrGPURefused) {
				t.Fatalf("switch = %v", err)
			}
			if len(r.calls) != 0 || !reflect.DeepEqual(r.gpuChecks, []string{"media"}) {
				t.Fatalf("calls = %v, checks = %v", r.calls, r.gpuChecks)
			}
			after, err := s.State(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if after.Version != before.Version || after.ActiveWorkload != control.WorkloadText || after.Health != control.HealthHealthy {
				t.Fatalf("blocked start changed state: %+v", after)
			}
		})
	}
}

func TestActivationRefusedWhileOtherProgramUsesGPU(t *testing.T) {
	s := openStore(t)
	state := ownershipState(t, s, control.OwnerSupervisor, control.WorkloadIdle)
	r := &fakeRuntime{active: control.WorkloadIdle, gpuErr: errGPUBusy}
	c := testController(t, s, r)
	snap, err := s.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	expected := control.OperatorPrecondition{Incarnation: state.LeaseFence.Incarnation, Version: state.Version, Owner: state.Owner, ConfigurationRevision: snap.Revision}
	if _, err := c.ActivateWorkload(context.Background(), control.WorkloadText, expected); !errors.Is(err, gpuruntime.ErrGPUInUse) {
		t.Fatalf("activation = %v", err)
	}
	if len(r.calls) != 0 {
		t.Fatalf("calls = %v", r.calls)
	}
	// The same precondition still works once the GPU is free: nothing was
	// committed.
	r.gpuErr = nil
	if _, err := c.ActivateWorkload(context.Background(), control.WorkloadText, expected); err != nil {
		t.Fatalf("retry = %v", err)
	}
}

// Transitions that start nothing never run the check: a running supervised
// workload is not affected.
func TestGPUCheckSkippedWhenNothingStarts(t *testing.T) {
	for _, target := range []control.Workload{control.WorkloadText, control.WorkloadIdle} {
		s := openStore(t)
		ownershipState(t, s, control.OwnerSupervisor, control.WorkloadText)
		r := &fakeRuntime{active: control.WorkloadText, gpuErr: errGPUBusy}
		c := testController(t, s, r)
		if _, err := c.Switch(context.Background(), target, "test"); err != nil {
			t.Fatalf("switch to %s = %v", target, err)
		}
		if len(r.gpuChecks) != 0 {
			t.Fatalf("checks = %v", r.gpuChecks)
		}
	}
}

func TestTrainingStartRefusedBeforeClaimWhileOtherProgramUsesGPU(t *testing.T) {
	c, s, r := trainingStartContract(t)
	r.gpuErr = errGPUBusy
	ctx := context.Background()
	if _, err := c.StartTraining(ctx, "job-b", "trainer"); !errors.Is(err, gpuruntime.ErrGPUInUse) {
		t.Fatalf("training start = %v", err)
	}
	if len(r.trainingCalls) != 0 || len(r.calls) != 0 {
		t.Fatalf("blocked trainer drove effects: %v %v", r.trainingCalls, r.calls)
	}
	if _, err := s.TrainingJob(ctx, "job-b"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("blocked trainer claimed a job: %v", err)
	}
}

func TestCloseGPUProcessPassesThroughRuntime(t *testing.T) {
	s := openStore(t)
	ownershipState(t, s, control.OwnerUser, control.WorkloadText)
	r := &fakeRuntime{active: control.WorkloadText, closeErr: errGPUBusy}
	c := testController(t, s, r)
	if err := c.CloseGPUProcess(context.Background(), control.WorkloadMedia, 7); !errors.Is(err, gpuruntime.ErrGPUInUse) {
		t.Fatalf("close = %v", err)
	}
	if !reflect.DeepEqual(r.gpuChecks, []string{"close media 7"}) {
		t.Fatalf("checks = %v", r.gpuChecks)
	}
	for _, target := range []control.Workload{control.WorkloadIdle, control.WorkloadUnknown, "vision"} {
		if err := c.CloseGPUProcess(context.Background(), target, 7); !errors.Is(err, ErrInvalidTarget) {
			t.Fatalf("close for %q = %v", target, err)
		}
	}
}

func TestFailureCodeNamesGPUCauses(t *testing.T) {
	if got := failureCode(errGPUBusy); got != "gpu-in-use" {
		t.Fatalf("in use = %q", got)
	}
	if got := failureCode(errGPUCheck); got != "gpu-check" {
		t.Fatalf("check = %q", got)
	}
}
