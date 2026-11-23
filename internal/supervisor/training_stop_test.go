package supervisor

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

type trainingStopRuntime struct {
	*trainingContractRuntime
	signals   []string
	signalErr error
	// exitAfter is the number of exit probes that report a running trainer.
	exitAfter int
	probes    int
}

func (r *trainingStopRuntime) SignalTraining(_ context.Context, _ control.WorkloadProfile, _ control.TrainingJob, signal string) error {
	r.signals = append(r.signals, signal)
	return r.signalErr
}

func (r *trainingStopRuntime) TrainingExited(context.Context, control.WorkloadProfile, control.TrainingJob) error {
	r.probes++
	if r.probes > r.exitAfter {
		return nil
	}
	return errors.New("trainer is still running")
}

func trainingStopContract(t *testing.T, exitAfter int) (*Controller, *trainingStopRuntime, control.TrainingJob) {
	t.Helper()
	_, s, base, job := trainingControllerContract(t)
	r := &trainingStopRuntime{trainingContractRuntime: base, exitAfter: exitAfter}
	c := testController(t, s, r)
	c.config.VerifyTimeout = 5 * time.Millisecond
	c.config.PollInterval = time.Millisecond
	return c, r, job
}

func TestStopTrainingWaitsForCheckpointExit(t *testing.T) {
	c, r, job := trainingStopContract(t, 2)
	stop, err := c.StopTraining(context.Background(), job.ID, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if stop.Escalated || !slices.Equal(r.signals, []string{gpuruntime.TrainingStopSignal}) {
		t.Fatalf("clean stop escalated=%v signals=%v", stop.Escalated, r.signals)
	}
	stored, err := c.store.TrainingJob(context.Background(), job.ID)
	if err != nil || stored.Status != "cancelled" {
		t.Fatalf("status=%s: %v", stored.Status, err)
	}
	assertCalls(t, r.calls, "stop trainer")
	assertCalls(t, r.trainingCalls, "peak", "release")
}

func TestStopTrainingEscalatesAfterGrace(t *testing.T) {
	c, r, job := trainingStopContract(t, 1<<30)
	stop, err := c.StopTraining(context.Background(), job.ID, 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !stop.Escalated || !slices.Equal(r.signals, []string{gpuruntime.TrainingStopSignal, gpuruntime.TrainingKillSignal}) {
		t.Fatalf("escalated=%v signals=%v", stop.Escalated, r.signals)
	}
	stored, err := c.store.TrainingJob(context.Background(), job.ID)
	if err != nil || stored.Status != "cancelled" {
		t.Fatalf("status=%s: %v", stored.Status, err)
	}
}

func TestStopTrainingKeepsJobWhenSignalFails(t *testing.T) {
	c, r, job := trainingStopContract(t, 0)
	r.signalErr = gpuruntime.ErrLaunchChanged
	if _, err := c.StopTraining(context.Background(), job.ID, time.Second); !errors.Is(err, gpuruntime.ErrLaunchChanged) {
		t.Fatalf("signal failure: %v", err)
	}
	stored, err := c.store.TrainingJob(context.Background(), job.ID)
	if err != nil || stored.Status != "running" || len(r.calls) != 0 {
		t.Fatalf("failed signal changed the job: status=%s calls=%v: %v", stored.Status, r.calls, err)
	}
}

func TestStopTrainingRejectsInvalidRequests(t *testing.T) {
	c, _, job := trainingStopContract(t, 0)
	if _, err := c.StopTraining(context.Background(), job.ID, 0); err == nil {
		t.Fatal("zero grace accepted")
	}
	plain, _, _ := trainingStopContract(t, 0)
	plain.runtime = &fakeRuntime{}
	if _, err := plain.StopTraining(context.Background(), job.ID, time.Second); err == nil {
		t.Fatal("runtime without signals accepted")
	}
}

func TestStopTrainingRacingCompletionStillVerifiesRelease(t *testing.T) {
	// The trainer finished before the signal arrived: the first exit probe
	// passes, and the job still ends only after release verification.
	c, r, job := trainingStopContract(t, 0)
	stop, err := c.StopTraining(context.Background(), job.ID, time.Second)
	if err != nil || stop.Escalated {
		t.Fatalf("escalated=%v: %v", stop.Escalated, err)
	}
	assertCalls(t, r.trainingCalls, "peak", "release")
}

func TestStopTrainingKeepsJobUntilReleaseIsProven(t *testing.T) {
	c, r, job := trainingStopContract(t, 0)
	r.releaseErr = errors.New("GPU compute processes remain after trainer exit")
	if _, err := c.StopTraining(context.Background(), job.ID, time.Second); err == nil {
		t.Fatal("stop finished without release evidence")
	}
	stored, err := c.store.TrainingJob(context.Background(), job.ID)
	if err != nil || stored.Status != "running" {
		t.Fatalf("unreleased job status=%s: %v", stored.Status, err)
	}
	// A restarted controller recovers the stopped job; it never relaunches.
	r.releaseErr = nil
	if _, err := testController(t, c.store, r).RecoverTraining(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(r.trainingCalls, "launch") {
		t.Fatal("recovery relaunched the trainer")
	}
}

// A job whose trainer exit is already recorded keeps that outcome and gets
// no signal; only its cleanup runs again.
func TestStopTrainingKeepsRecordedOutcome(t *testing.T) {
	c, r, job := trainingStopContract(t, 0)
	ctx := context.Background()
	evidence := control.TrainingTerminal{Version: 1, ID: job.ID, Token: job.Token, Fence: job.Fence, RecipeSHA256: job.RecipeSHA256, InvocationID: job.InvocationID, Outcome: "completed"}
	if err := c.store.RecordTrainingTerminal(ctx, job, evidence); err != nil {
		t.Fatal(err)
	}
	stop, err := c.StopTraining(ctx, job.ID, time.Second)
	if err != nil || stop.Escalated || len(r.signals) != 0 {
		t.Fatalf("stop = %+v signals=%v: %v", stop, r.signals, err)
	}
	stored, err := c.store.TrainingJob(ctx, job.ID)
	if err != nil || stored.Status != "completed" {
		t.Fatalf("status=%s: %v", stored.Status, err)
	}
}
