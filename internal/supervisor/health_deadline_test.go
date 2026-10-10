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
	healthProbe      func(context.Context, control.Workload)
	observationProbe func(context.Context)
}

func (r *blockedHealthRuntime) Healthy(ctx context.Context, target control.Workload) error {
	if !r.block {
		return nil
	}
	if r.healthProbe != nil {
		r.healthProbe(ctx, target)
	}
	<-ctx.Done()
	return ctx.Err()
}

func (r *blockedHealthRuntime) Observe(ctx context.Context) (gpuruntime.Snapshot, error) {
	if r.observationProbe != nil {
		r.observationProbe(ctx)
	}
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
			c.config.VerifyTimeout = 40 * time.Millisecond
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
	type probeContext struct {
		ctx     context.Context
		entered time.Time
		target  control.Workload
	}
	var healthProbes []probeContext
	var targetHealth context.Context
	var rollbackObservation *probeContext
	runtime.healthProbe = func(ctx context.Context, target control.Workload) {
		healthProbes = append(healthProbes, probeContext{ctx: ctx, entered: time.Now(), target: target})
		if target == control.WorkloadMedia {
			targetHealth = ctx
		}
	}
	runtime.observationProbe = func(ctx context.Context) {
		if targetHealth != nil && targetHealth.Err() != nil && rollbackObservation == nil {
			rollbackObservation = &probeContext{ctx: ctx, entered: time.Now()}
		}
	}
	// Switch also performs durable journal/phase writes and finalization. Assert
	// the actual callback deadlines instead of timing that unrelated SQLite work.
	// ActionTimeout still bounds the blocked callback if verification regresses.
	_, err := c.Switch(context.Background(), control.WorkloadMedia, "deadline-test")
	if !errors.Is(err, ErrVerifyTimeout) {
		t.Fatalf("switch error = %v", err)
	}
	assertBudget := func(name string, probe probeContext, budget time.Duration) {
		t.Helper()
		deadline, ok := probe.ctx.Deadline()
		if !ok || deadline.After(probe.entered.Add(budget)) {
			t.Fatalf("%s deadline = %v (present %v), want at most %v after callback entry %v", name, deadline, ok, budget, probe.entered)
		}
	}
	if len(healthProbes) == 0 || healthProbes[0].target != control.WorkloadMedia {
		t.Fatal("target health verification was not reached")
	}
	for _, probe := range healthProbes {
		budget := c.config.VerifyTimeout
		if probe.target == control.WorkloadText {
			budget = c.config.CleanupTimeout
		}
		assertBudget("blocked health "+string(probe.target), probe, budget)
		if !errors.Is(probe.ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("blocked health %s cancellation = %v, want deadline exceeded", probe.target, probe.ctx.Err())
		}
	}
	// After target health expires, rollback observes before its journal writes.
	// Its health probe may never run if those writes consume the cleanup budget.
	if rollbackObservation == nil {
		t.Fatal("rollback observation was not reached after target health expired")
	}
	assertBudget("rollback observation", *rollbackObservation, c.config.CleanupTimeout)
	verifyDeadline, _ := targetHealth.Deadline()
	rollbackDeadline, _ := rollbackObservation.ctx.Deadline()
	if !rollbackDeadline.After(verifyDeadline) {
		t.Fatal("rollback inherited the expired verification deadline")
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
