package supervisor

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

type fakeRuntime struct {
	active         control.Workload
	mediaReady     bool
	healthFailures int
	startErr       error
	stopErr        error
	releaseFailures int
	cancelOnStop   func()
	calls          []string
}

func (r *fakeRuntime) Observe(context.Context) (gpuruntime.Snapshot, error) {
	return gpuruntime.Snapshot{
		TextActive: r.active == control.WorkloadText,
		MediaReady: r.mediaReady,
	}, nil
}

func (r *fakeRuntime) Start(_ context.Context, workload control.Workload) error {
	r.calls = append(r.calls, "start "+string(workload))
	if r.startErr != nil && workload == control.WorkloadMedia {
		return r.startErr
	}
	if workload == control.WorkloadText {
		r.active = control.WorkloadText
	} else {
		r.active = control.WorkloadMedia
		r.mediaReady = true
	}
	return nil
}

func (r *fakeRuntime) Stop(_ context.Context, workload control.Workload) error {
	r.calls = append(r.calls, "stop "+string(workload))
	if r.cancelOnStop != nil {
		r.cancelOnStop()
	}
	if r.stopErr != nil {
		return r.stopErr
	}
	if r.active == workload {
		r.active = control.WorkloadIdle
	}
	return nil
}

func (r *fakeRuntime) Released(context.Context) error {
	if r.releaseFailures > 0 {
		r.releaseFailures--
		return errors.New("GPU memory not released")
	}
	return nil
}

func (r *fakeRuntime) Healthy(context.Context, control.Workload) error {
	if r.healthFailures > 0 {
		r.healthFailures--
		return errors.New("not ready")
	}
	return nil
}

func TestSwitchStopsTextBeforeStartingMedia(t *testing.T) {
	stateStore := openStore(t)
	runtime := &fakeRuntime{active: control.WorkloadText, mediaReady: true}
	controller := testController(t, stateStore, runtime)
	if _, err := controller.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := controller.Switch(context.Background(), control.WorkloadMedia, "test")
	if err != nil {
		t.Fatal(err)
	}
	if state.ActiveWorkload != control.WorkloadMedia || state.Admission != control.AdmissionOpen {
		t.Fatalf("state = %#v", state)
	}
	assertCalls(t, runtime.calls, "stop text", "start media")
	events, err := stateStore.TransitionEvents(context.Background(), "11111111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 ||
		events[0].Kind != "intent" || events[0].Action != "stop text" ||
		events[1].Kind != "observation" || events[1].Outcome != "ok" ||
		events[2].Kind != "intent" || events[2].Action != "start media" ||
		events[3].Kind != "observation" || events[3].Outcome != "ok" {
		t.Fatalf("events = %#v", events)
	}
}

func TestSwitchFailureRollsBackAndLatchesError(t *testing.T) {
	stateStore := openStore(t)
	runtime := &fakeRuntime{active: control.WorkloadText, mediaReady: true, startErr: errors.New("start failed")}
	controller := testController(t, stateStore, runtime)
	if _, err := controller.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := controller.Switch(context.Background(), control.WorkloadMedia, "test")
	if err == nil {
		t.Fatal("expected switch failure")
	}
	if state.ActiveWorkload != control.WorkloadText || state.Health != control.HealthError || state.Admission != control.AdmissionClosed {
		t.Fatalf("unsafe failed state = %#v", state)
	}
	assertCalls(t, runtime.calls, "stop text", "start media", "start text")
}

func TestReadinessPollingAllowsDelayedHealth(t *testing.T) {
	stateStore := openStore(t)
	runtime := &fakeRuntime{active: control.WorkloadText, mediaReady: true}
	controller := testController(t, stateStore, runtime)
	if _, err := controller.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	runtime.healthFailures = 2
	state, err := controller.Switch(context.Background(), control.WorkloadMedia, "test")
	if err != nil {
		t.Fatal(err)
	}
	if state.ActiveWorkload != control.WorkloadMedia {
		t.Fatalf("state = %#v", state)
	}
}

func TestInterruptedTransitionRequiresExplicitRecovery(t *testing.T) {
	stateStore := openStore(t)
	runtime := &fakeRuntime{active: control.WorkloadText, mediaReady: true}
	controller := testController(t, stateStore, runtime)
	current, err := controller.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	target := current
	target.DesiredWorkload = control.WorkloadMedia
	_, err = stateStore.StartTransition(context.Background(), current.Version, store.Transition{
		ID: "interrupted", Source: current, Target: target, Previous: current,
		Initiator: "test", Phase: control.PhaseDraining, Deadline: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Reconcile(context.Background()); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("reconcile error = %v", err)
	}
	if running, err := stateStore.InProgressTransition(context.Background()); err != nil || running != "" {
		t.Fatalf("running transition = %q, error = %v", running, err)
	}
	if _, err := controller.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Switch(context.Background(), control.WorkloadMedia, "test"); err != nil {
		t.Fatal(err)
	}
}

func TestUserOwnershipBlocksSupervisorSwitchAndRecovery(t *testing.T) {
	stateStore := openStore(t)
	runtime := &fakeRuntime{mediaReady: true}
	controller := testController(t, stateStore, runtime)
	state, err := stateStore.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state.Owner = control.OwnerUser
	state.Phase = control.PhaseStable
	if _, err := stateStore.UpdateState(context.Background(), state.Version, state); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Switch(context.Background(), control.WorkloadText, "test"); !errors.Is(err, ErrUserOwned) {
		t.Fatalf("switch error = %v", err)
	}
	if _, err := controller.Recover(context.Background()); !errors.Is(err, ErrUserOwned) {
		t.Fatalf("recover error = %v", err)
	}
	if len(runtime.calls) != 0 {
		t.Fatalf("runtime calls = %#v", runtime.calls)
	}
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	stateStore, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stateStore.Close() })
	return stateStore
}

func testController(t *testing.T, stateStore StateStore, runtime gpuruntime.Manager) *Controller {
	t.Helper()
	controller, err := newController(stateStore, runtime, Config{
		DrainTimeout: time.Second, VerifyTimeout: time.Second,
		CleanupTimeout: time.Second, PollInterval: time.Millisecond,
	}, time.Now, func() (string, error) {
		return "11111111-1111-4111-8111-111111111111", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return controller
}

func assertCalls(t *testing.T, actual []string, expected ...string) {
	t.Helper()
	if len(actual) != len(expected) {
		t.Fatalf("calls = %#v", actual)
	}
	for index := range expected {
		if actual[index] != expected[index] {
			t.Fatalf("calls = %#v", actual)
		}
	}
}

func TestSwitchToIdleReleasesMediaWithoutStoppingUI(t *testing.T) {
	stateStore := openStore(t)
	runtime := &fakeRuntime{active: control.WorkloadMedia, mediaReady: true}
	controller := testController(t, stateStore, runtime)
	state, err := stateStore.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state.Owner = control.OwnerSupervisor
	state.DesiredWorkload = control.WorkloadMedia
	state.ActiveWorkload = control.WorkloadMedia
	state.Phase = control.PhaseStable
	state.Admission = control.AdmissionOpen
	state, err = stateStore.UpdateState(context.Background(), state.Version, state)
	if err != nil {
		t.Fatal(err)
	}
	result, err := controller.Switch(context.Background(), control.WorkloadIdle, "test")
	if err != nil {
		t.Fatal(err)
	}
	if result.ActiveWorkload != control.WorkloadIdle || result.Admission != control.AdmissionClosed {
		t.Fatalf("state = %#v", result)
	}
	assertCalls(t, runtime.calls, "stop media")
}

func TestSwitchWaitsForGPUReleaseBeforeStartingText(t *testing.T) {
	stateStore := openStore(t)
	runtime := &fakeRuntime{active: control.WorkloadMedia, mediaReady: true, releaseFailures: 2}
	controller := testController(t, stateStore, runtime)
	state, err := stateStore.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state.ActiveWorkload = control.WorkloadMedia
	state.DesiredWorkload = control.WorkloadMedia
	state.Phase = control.PhaseStable
	state.Health = control.HealthHealthy
	state.Admission = control.AdmissionOpen
	if _, err := stateStore.UpdateState(context.Background(), state.Version, state); err != nil {
		t.Fatal(err)
	}
	result, err := controller.Switch(context.Background(), control.WorkloadText, "test")
	if err != nil {
		t.Fatal(err)
	}
	if result.ActiveWorkload != control.WorkloadText {
		t.Fatalf("active workload = %q", result.ActiveWorkload)
	}
	assertCalls(t, runtime.calls, "stop media", "start text")
}

func TestCanceledSwitchClosesTransitionAfterRollbackTimeout(t *testing.T) {
	stateStore := openStore(t)
	runtime := &fakeRuntime{active: control.WorkloadText, mediaReady: true}
	controller := testController(t, stateStore, runtime)
	if _, err := controller.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runtime.cancelOnStop = cancel
	if _, err := controller.Switch(ctx, control.WorkloadMedia, "test"); err == nil {
		t.Fatal("expected canceled switch")
	}
	running, err := stateStore.InProgressTransition(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if running != "" {
		t.Fatalf("transition remains in progress: %s", running)
	}
}
