package supervisor

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

type exclusiveRuntime struct{ fakeRuntime }

func (r *exclusiveRuntime) Observe(ctx context.Context) (gpuruntime.Snapshot, error) {
	s, err := r.fakeRuntime.Observe(ctx)
	s.MediaExclusive = true
	if s.TextActive && s.MediaReady {
		return s, errors.New("concurrent runtimes")
	}
	return s, err
}
func (r *exclusiveRuntime) Stop(ctx context.Context, workload control.Workload) error {
	if err := r.fakeRuntime.Stop(ctx, workload); err != nil {
		return err
	}
	if workload == control.WorkloadMedia {
		r.mediaReady = false
	}
	return nil
}
func (r *exclusiveRuntime) Start(ctx context.Context, workload control.Workload) error {
	if (workload == control.WorkloadText && r.mediaReady) || (workload == control.WorkloadMedia && r.active == control.WorkloadText) {
		return errors.New("opposing runtime active")
	}
	return r.fakeRuntime.Start(ctx, workload)
}

func TestExclusiveMediaTransitionsAndOwnership(t *testing.T) {
	ctx := context.Background()
	stateStore := openStore(t)
	runtime := &exclusiveRuntime{}
	controller := testController(t, stateStore, runtime)
	controller.id = func() (string, error) { return uuid.NewString(), nil }
	if _, err := controller.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	operations := []func() (control.State, error){
		func() (control.State, error) { return controller.Switch(ctx, control.WorkloadMedia, "test") },
		func() (control.State, error) { return controller.Switch(ctx, control.WorkloadText, "test") },
		func() (control.State, error) { return controller.TransferToUser(ctx, control.WorkloadMedia, "test") },
		func() (control.State, error) { return controller.SwitchUser(ctx, control.WorkloadText, "test") },
		func() (control.State, error) { return controller.SwitchUser(ctx, control.WorkloadIdle, "test") },
		func() (control.State, error) {
			return controller.TransferToSupervisor(ctx, control.WorkloadMedia, "test")
		},
		func() (control.State, error) { return controller.Switch(ctx, control.WorkloadIdle, "test") },
	}
	for i, operation := range operations {
		state, err := operation()
		if err != nil {
			t.Fatalf("operation %d: %v", i, err)
		}
		if state.ActiveWorkload != control.WorkloadMedia && runtime.mediaReady {
			t.Fatalf("operation %d left media running", i)
		}
	}
}

func TestExclusiveReconciliationAndRecoveryStopUnexpectedMedia(t *testing.T) {
	for _, recover := range []bool{false, true} {
		stateStore := openStore(t)
		runtime := &exclusiveRuntime{fakeRuntime: fakeRuntime{mediaReady: true}}
		controller := testController(t, stateStore, runtime)
		var state control.State
		var err error
		if recover {
			state, err = controller.Recover(context.Background())
		} else {
			state, err = controller.Reconcile(context.Background())
		}
		if err != nil || runtime.mediaReady || state.ActiveWorkload != control.WorkloadIdle || state.Admission != control.AdmissionClosed {
			t.Fatalf("recover=%v state=%#v err=%v", recover, state, err)
		}
	}
}

func TestExclusiveIdleSwitchStopsUnexpectedMedia(t *testing.T) {
	stateStore := openStore(t)
	runtime := &exclusiveRuntime{}
	controller := testController(t, stateStore, runtime)
	if _, err := controller.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	runtime.mediaReady = true
	state, err := controller.Switch(context.Background(), control.WorkloadIdle, "test")
	if err != nil || runtime.mediaReady || state.ActiveWorkload != control.WorkloadIdle {
		t.Fatalf("state=%#v err=%v", state, err)
	}
}

func TestExclusiveVerificationRejectsMediaDuringTextAndIdle(t *testing.T) {
	for _, target := range []control.Workload{control.WorkloadText, control.WorkloadIdle} {
		if err := verifySnapshot(target, gpuruntime.Snapshot{TextActive: target == control.WorkloadText, MediaReady: true, MediaExclusive: true}); err == nil {
			t.Fatal("accepted media for", target)
		}
	}
}

func TestExclusiveFailedMediaStartDoesNotRestartTextAlongsideMedia(t *testing.T) {
	ctx := context.Background()
	stateStore := openStore(t)
	runtime := &exclusiveRuntime{fakeRuntime: fakeRuntime{active: control.WorkloadText}}
	controller := testController(t, stateStore, runtime)
	if _, err := controller.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	runtime.startErr = errors.New("partial start")
	runtime.partialStart = true
	state, err := controller.Switch(ctx, control.WorkloadMedia, "test")
	if err == nil || state.Admission != control.AdmissionClosed || state.Health != control.HealthError || runtime.active == control.WorkloadText {
		t.Fatalf("state=%#v runtime=%#v err=%v", state, runtime, err)
	}
	if _, err := controller.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if runtime.mediaReady {
		t.Fatal("recovery left media active")
	}
}

func TestExclusiveUserRecoveryRejectsWrongWorkloadWithoutStoppingUserWork(t *testing.T) {
	ctx := context.Background()
	stateStore := openStore(t)
	runtime := &exclusiveRuntime{}
	controller := testController(t, stateStore, runtime)
	controller.id = func() (string, error) { return uuid.NewString(), nil }
	controller.config.VerifyTimeout = time.Millisecond
	if _, err := controller.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.TransferToUser(ctx, control.WorkloadMedia, "test"); err != nil {
		t.Fatal(err)
	}
	runtime.calls = nil
	state, err := controller.RecoverUser(ctx, control.WorkloadIdle, "test")
	if err == nil || state.Owner != control.OwnerUser || state.Admission != control.AdmissionClosed || !runtime.mediaReady || len(runtime.calls) != 0 {
		t.Fatalf("state=%#v calls=%v err=%v", state, runtime.calls, err)
	}
}

func TestExclusiveConcurrentBootFailsClosed(t *testing.T) {
	runtime := &exclusiveRuntime{fakeRuntime: fakeRuntime{active: control.WorkloadText, mediaReady: true}}
	controller := testController(t, openStore(t), runtime)
	state, err := controller.Reconcile(context.Background())
	if err == nil || state.Admission != control.AdmissionClosed || state.Health != control.HealthError {
		t.Fatalf("state=%#v err=%v", state, err)
	}
}

type delayedExclusiveRuntime struct{ exclusiveRuntime }

func (r *delayedExclusiveRuntime) Start(ctx context.Context, workload control.Workload) error {
	if err := r.ReleasedFor(ctx, control.WorkloadIdle); err != nil {
		return err
	}
	return r.exclusiveRuntime.Start(ctx, workload)
}

func TestExclusiveTextToMediaWaitsForGPURelease(t *testing.T) {
	for _, failures := range []int{1, 1000000} {
		t.Run(fmt.Sprint(failures), func(t *testing.T) {
			ctx := context.Background()
			runtime := &delayedExclusiveRuntime{exclusiveRuntime: exclusiveRuntime{fakeRuntime: fakeRuntime{active: control.WorkloadText}}}
			controller := testController(t, openStore(t), runtime)
			controller.config.VerifyTimeout = 5 * time.Millisecond
			controller.config.CleanupTimeout = 5 * time.Millisecond
			if _, err := controller.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			runtime.releaseFailures = failures
			state, err := controller.Switch(ctx, control.WorkloadMedia, "test")
			if failures == 1 {
				if err != nil || state.ActiveWorkload != control.WorkloadMedia {
					t.Fatalf("state=%#v err=%v", state, err)
				}
				assertCalls(t, runtime.calls, "stop text", "start media")
			} else {
				if !errors.Is(err, ErrVerifyTimeout) || state.Admission != control.AdmissionClosed || state.Health != control.HealthError {
					t.Fatalf("state=%#v err=%v", state, err)
				}
				assertCalls(t, runtime.calls, "stop text")
			}
		})
	}
}
