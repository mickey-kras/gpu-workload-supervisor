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
	active    control.Workload
	healthErr error
	startErr  error
	stopErr   error
	calls     []string
}

func (r *fakeRuntime) Observe(context.Context) (gpuruntime.Snapshot, error) {
	return gpuruntime.Snapshot{
		TextActive:  r.active == control.WorkloadText,
		MediaActive: r.active == control.WorkloadMedia,
	}, nil
}

func (r *fakeRuntime) Start(_ context.Context, workload control.Workload) error {
	r.calls = append(r.calls, "start "+string(workload))
	if r.startErr == nil {
		r.active = workload
	}
	return r.startErr
}

func (r *fakeRuntime) Stop(_ context.Context, workload control.Workload) error {
	r.calls = append(r.calls, "stop "+string(workload))
	if r.stopErr == nil && r.active == workload {
		r.active = control.WorkloadIdle
	}
	return r.stopErr
}

func (r *fakeRuntime) Healthy(context.Context, control.Workload) error {
	return r.healthErr
}

func TestSwitchStopsTextBeforeStartingMedia(t *testing.T) {
	stateStore := openStore(t)
	runtime := &fakeRuntime{active: control.WorkloadText}
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
	want := []string{"stop text", "start media"}
	if len(runtime.calls) != len(want) {
		t.Fatalf("calls = %#v", runtime.calls)
	}
	for index := range want {
		if runtime.calls[index] != want[index] {
			t.Fatalf("calls = %#v", runtime.calls)
		}
	}
}

func TestSwitchFailureLatchesClosedError(t *testing.T) {
	stateStore := openStore(t)
	runtime := &fakeRuntime{active: control.WorkloadText, startErr: errors.New("start failed")}
	controller := testController(t, stateStore, runtime)
	if _, err := controller.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := controller.Switch(context.Background(), control.WorkloadMedia, "test")
	if err == nil {
		t.Fatal("expected switch failure")
	}
	if state.Health != control.HealthError || state.Admission != control.AdmissionClosed {
		t.Fatalf("unsafe failed state = %#v", state)
	}
}

func TestUserOwnershipBlocksSupervisorSwitch(t *testing.T) {
	stateStore := openStore(t)
	runtime := &fakeRuntime{active: control.WorkloadIdle}
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
		DrainTimeout: time.Second,
		PollInterval: time.Millisecond,
	}, time.Now, func() (string, error) {
		return "11111111-1111-4111-8111-111111111111", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return controller
}
