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
	if r.startErr != nil {
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
	if r.stopErr != nil {
		return r.stopErr
	}
	if r.active == workload {
		r.active = control.WorkloadIdle
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
