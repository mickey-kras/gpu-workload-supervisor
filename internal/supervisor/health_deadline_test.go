package supervisor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

type blockedHealthRuntime struct {
	fakeRuntime
	block            bool
	blockObservation bool
}

func (r *blockedHealthRuntime) Healthy(ctx context.Context, _ control.Workload) error {
	if !r.block {
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
}

func (r *blockedHealthRuntime) Observe(ctx context.Context) (gpuruntime.Snapshot, error) {
	if r.blockObservation {
		<-ctx.Done()
		return gpuruntime.Snapshot{}, ctx.Err()
	}
	return r.fakeRuntime.Observe(ctx)
}

func TestHealthDeadlineReconcileAndRecover(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		name := "reconcile"
		if recovery {
			name = "recover"
		}
		t.Run(name, func(t *testing.T) {
			stateStore := openStore(t)
			runtime := &blockedHealthRuntime{fakeRuntime: fakeRuntime{active: control.WorkloadText}}
			c := testController(t, stateStore, runtime)
			if _, err := c.Reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			if recovery {
				state, err := stateStore.State(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if _, err := c.latchObservationFailure(context.Background(), state, errors.New("failure")); err == nil {
					t.Fatal("expected latch error")
				}
			}
			var err error
			runtime.block = true
			c.config.ActionTimeout = 20 * time.Millisecond
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			start := time.Now()
			if recovery {
				_, err = c.Recover(ctx)
			} else {
				_, err = c.Reconcile(ctx)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("health error = %v", err)
			}
			if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
				t.Fatalf("health probe exceeded action budget: %v", elapsed)
			}
			after, err := stateStore.State(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if after.Admission != control.AdmissionClosed || after.Health != control.HealthError {
				t.Fatalf("timeout did not fail closed: %#v", after)
			}

		})
	}
}

func TestSwitchHealthDeadlineFailsClosed(t *testing.T) {
	stateStore := openStore(t)
	runtime := &blockedHealthRuntime{fakeRuntime: fakeRuntime{active: control.WorkloadText}}
	c := testController(t, stateStore, runtime)
	if _, err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	runtime.block = true
	c.config.VerifyTimeout = 20 * time.Millisecond
	c.config.ActionTimeout = time.Second
	c.config.CleanupTimeout = 30 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Switch(ctx, control.WorkloadMedia, "deadline-test")
	if !errors.Is(err, ErrVerifyTimeout) {
		t.Fatalf("switch error = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("switch health exceeded verification and cleanup budgets: %v", elapsed)
	}
	state, err := stateStore.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Admission != control.AdmissionClosed || state.Health != control.HealthError || state.Phase != control.PhaseReconciling {
		t.Fatalf("timeout did not fail closed: %#v", state)
	}
	running, err := stateStore.InProgressTransition(context.Background())
	if err != nil || running != "" {
		t.Fatalf("transition not finalized: %q, %v", running, err)
	}
}

func TestReadinessDeadlineBoundsObservation(t *testing.T) {
	c := testController(t, openStore(t), &blockedHealthRuntime{blockObservation: true})
	c.config.ActionTimeout = 500 * time.Millisecond
	start := time.Now()
	err := c.waitReady(context.Background(), control.WorkloadText, time.Now().Add(20*time.Millisecond))
	if !errors.Is(err, ErrVerifyTimeout) || !errors.Is(err, ErrRuntimeObservation) {
		t.Fatalf("readiness error = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("observation exceeded overall readiness budget: %v", elapsed)
	}
}

func TestReadinessRejectsSuccessAfterDeadline(t *testing.T) {
	c := testController(t, openStore(t), &fakeRuntime{active: control.WorkloadText})
	if err := c.waitReady(context.Background(), control.WorkloadText, time.Now().Add(-time.Second)); !errors.Is(err, ErrVerifyTimeout) {
		t.Fatalf("expired readiness succeeded: %v", err)
	}
}
