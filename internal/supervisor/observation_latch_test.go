package supervisor

import (
	"context"
	"errors"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

type cancelingObserveRuntime struct {
	*fakeRuntime
	cancel context.CancelFunc
}

func (r *cancelingObserveRuntime) Observe(ctx context.Context) (gpuruntime.Snapshot, error) {
	if r.cancel != nil {
		r.cancel()
		return gpuruntime.Snapshot{}, ctx.Err()
	}
	return r.fakeRuntime.Observe(ctx)
}

func TestStatusObservationCancellationPersistsSafetyLatch(t *testing.T) {
	stateStore := openStore(t)
	runtime := &cancelingObserveRuntime{fakeRuntime: &fakeRuntime{active: control.WorkloadText, mediaReady: true}}
	controller := testController(t, stateStore, runtime)
	if _, err := controller.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtime.cancel = cancel
	if _, err := controller.Status(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("status error = %v", err)
	}
	persisted, err := stateStore.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Health != control.HealthError || persisted.Admission != control.AdmissionClosed ||
		persisted.Phase != control.PhaseReconciling || persisted.ActiveWorkload != control.WorkloadUnknown {
		t.Fatalf("unsafe persisted state = %#v", persisted)
	}
}

func TestReconcileObservationCancellationPersistsSafetyLatch(t *testing.T) {
	stateStore := openStore(t)
	runtime := &cancelingObserveRuntime{fakeRuntime: &fakeRuntime{active: control.WorkloadText, mediaReady: true}}
	controller := testController(t, stateStore, runtime)
	if _, err := controller.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtime.cancel = cancel
	if _, err := controller.Reconcile(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("reconcile error = %v", err)
	}
	persisted, err := stateStore.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Health != control.HealthError || persisted.Admission != control.AdmissionClosed ||
		persisted.Phase != control.PhaseReconciling || persisted.ActiveWorkload != control.WorkloadUnknown {
		t.Fatalf("unsafe persisted state = %#v", persisted)
	}
}
