package supervisor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

func TestTextRecoveryWaitsForOldMediaRegistration(t *testing.T) {
	for _, tc := range []struct {
		name        string
		run         func(*Controller, context.Context) (control.State, error)
		durableText bool
	}{
		{"reconcile", (*Controller).Reconcile, false},
		{"recover", (*Controller).Recover, false},
		{"reconcile with durable text", (*Controller).Reconcile, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			stateStore := openStore(t)
			runtime := &fakeRuntime{active: control.WorkloadText, mediaReady: true}
			controller := testController(t, stateStore, runtime)
			controller.config.DrainTimeout = 10 * time.Millisecond
			state, err := stateStore.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			state.ActiveWorkload = control.WorkloadMedia
			state.DesiredWorkload = control.WorkloadMedia
			state.Phase = control.PhaseStable
			state.Health = control.HealthHealthy
			state.Admission = control.AdmissionOpen
			state, err = stateStore.UpdateState(ctx, state.Version, state)
			if err != nil {
				t.Fatal(err)
			}
			token, err := stateStore.AdmitWorkToken(ctx, "media-pending", "", control.WorkloadMedia, state.LeaseFence)
			if err != nil {
				t.Fatal(err)
			}
			if tc.durableText {
				updated := state
				updated.ActiveWorkload = control.WorkloadText
				updated.DesiredWorkload = control.WorkloadText
				if _, err := stateStore.UpdateState(ctx, state.Version, updated); err != nil {
					t.Fatal(err)
				}
			}
			result, err := tc.run(controller, ctx)
			if !errors.Is(err, ErrDrainTimeout) {
				t.Fatalf("error = %v, state = %#v", err, result)
			}
			persisted, err := stateStore.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if persisted != result || persisted.Admission != control.AdmissionClosed ||
				persisted.Health != control.HealthError || persisted.Phase != control.PhaseReconciling {
				t.Fatalf("recovery did not stay closed: returned %#v, persisted %#v", result, persisted)
			}
			if pending, err := stateStore.PendingWork(ctx); err != nil || pending != 1 {
				t.Fatalf("pending work = %d, %v", pending, err)
			}
			if len(runtime.calls) != 0 {
				t.Fatalf("stopped runtime while work remained: %#v", runtime.calls)
			}
			if err := stateStore.FinishWorkToken(ctx, "media-pending", control.WorkloadMedia, state.LeaseFence, token, store.WorkCompleted); err != nil {
				t.Fatal(err)
			}
			result, err = controller.Recover(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if result.ActiveWorkload != control.WorkloadText || result.Admission != control.AdmissionOpen ||
				result.Health != control.HealthHealthy || result.Phase != control.PhaseStable {
				t.Fatalf("recovered state = %#v", result)
			}
			if len(runtime.calls) != 0 {
				t.Fatalf("changed healthy text runtime: %#v", runtime.calls)
			}
		})
	}
}

func TestReconcileHealthyTextPreservesPendingTextWork(t *testing.T) {
	ctx := context.Background()
	stateStore := openStore(t)
	runtime := &fakeRuntime{active: control.WorkloadText, mediaReady: true}
	controller := testController(t, stateStore, runtime)
	state, err := stateStore.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state.ActiveWorkload = control.WorkloadText
	state.DesiredWorkload = control.WorkloadText
	state.Phase = control.PhaseStable
	state.Health = control.HealthHealthy
	state.Admission = control.AdmissionOpen
	state, err = stateStore.UpdateState(ctx, state.Version, state)
	if err != nil {
		t.Fatal(err)
	}
	token, err := stateStore.AdmitWorkToken(ctx, "text-pending", "", control.WorkloadText, state.LeaseFence)
	if err != nil {
		t.Fatal(err)
	}
	result, err := controller.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Version != state.Version+1 || result.ActiveWorkload != control.WorkloadText ||
		result.Admission != control.AdmissionOpen || result.Health != control.HealthHealthy {
		t.Fatalf("healthy text changed unexpectedly: %#v", result)
	}
	if pending, err := stateStore.PendingWork(ctx); err != nil || pending != 1 {
		t.Fatalf("pending text work = %d, %v", pending, err)
	}
	if err := stateStore.FinishWorkToken(ctx, "text-pending", control.WorkloadText, state.LeaseFence, token, store.WorkCompleted); err != nil {
		t.Fatal(err)
	}
}
