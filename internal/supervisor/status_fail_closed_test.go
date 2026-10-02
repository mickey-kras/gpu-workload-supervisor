package supervisor

import (
	"context"
	"errors"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestStatusLatchesOpenAdmissionWhenRuntimeStops(t *testing.T) {
	ctx := context.Background()
	stateStore := openStore(t)
	runtime := &fakeRuntime{active: control.WorkloadText, mediaReady: true}
	controller := testController(t, stateStore, runtime)
	healthy, err := controller.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	runtime.active = control.WorkloadIdle

	got, err := controller.Status(ctx)
	if !errors.Is(err, ErrStateVerification) {
		t.Fatalf("status error = %v", err)
	}
	if got.Version <= healthy.Version || got.ActiveWorkload != control.WorkloadUnknown ||
		got.Health != control.HealthError || got.Admission != control.AdmissionClosed ||
		got.Phase != control.PhaseReconciling {
		t.Fatalf("unsafe status = %#v", got)
	}
	persisted, err := stateStore.State(ctx)
	if err != nil || persisted != got {
		t.Fatalf("persisted state = %#v, error = %v", persisted, err)
	}
	if len(runtime.calls) != 0 {
		t.Fatalf("status changed runtime: %#v", runtime.calls)
	}
	if again, err := controller.Status(ctx); err != nil ||
		again.Health != control.HealthError || again.Admission != control.AdmissionClosed {
		t.Fatalf("second status = %#v, error = %v", again, err)
	}
}

func TestStatusLatchesOpenAdmissionWhenRuntimeUnhealthy(t *testing.T) {
	ctx := context.Background()
	stateStore := openStore(t)
	runtime := &fakeRuntime{active: control.WorkloadText, mediaReady: true}
	controller := testController(t, stateStore, runtime)
	if _, err := controller.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	runtime.healthFailures = 1

	got, err := controller.Status(ctx)
	if !errors.Is(err, ErrHealthCheck) {
		t.Fatalf("status error = %v", err)
	}
	if got.ActiveWorkload != control.WorkloadUnknown || got.Health != control.HealthError ||
		got.Admission != control.AdmissionClosed || got.Phase != control.PhaseReconciling {
		t.Fatalf("unsafe status = %#v", got)
	}
	persisted, err := stateStore.State(ctx)
	if err != nil || persisted != got {
		t.Fatalf("persisted state = %#v, error = %v", persisted, err)
	}
	if len(runtime.calls) != 0 {
		t.Fatalf("status changed runtime: %#v", runtime.calls)
	}
}

func TestStatusKeepsHealthyOpenAdmissionWhenRuntimeMatches(t *testing.T) {
	ctx := context.Background()
	stateStore := openStore(t)
	runtime := &fakeRuntime{active: control.WorkloadText, mediaReady: true}
	controller := testController(t, stateStore, runtime)
	before, err := controller.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, err := controller.Status(ctx)
	if err != nil || got != before {
		t.Fatalf("status = %#v, error = %v; before = %#v", got, err, before)
	}
}
