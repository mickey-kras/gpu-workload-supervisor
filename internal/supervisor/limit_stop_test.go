package supervisor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

// stopsAfterStart counts the stops of vision after its start.
func stopsAfterStart(r *limitedRuntime) int {
	started := slices.Index(r.calls, "start vision")
	if started < 0 {
		return 0
	}
	n := 0
	for _, call := range r.calls[started:] {
		if call == "stop vision" {
			n++
		}
	}
	return n
}

// limitStopEvents returns the journaled limit stop events of vision as
// kind/outcome pairs.
func limitStopEvents(t *testing.T, r *limitedRuntime) []string {
	t.Helper()
	var events []string
	for n := 1; n < 20; n++ {
		list, err := transitionEvents(r.statePath, context.Background(), fmt.Sprintf("11111111-1111-4111-8111-%012d", n))
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range list {
			if event.Action == "limit stop vision" {
				events = append(events, event.Kind+"/"+event.Outcome)
			}
		}
	}
	return events
}

// userOwnedText gives a controller whose user owns the running text.
func userOwnedText(t *testing.T) (*Controller, *limitedRuntime, storeGateway) {
	t.Helper()
	c, r, s := limitedController(t, limitedCatalog())
	ctx := context.Background()
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.TransferToUser(ctx, control.WorkloadText, "test"); err != nil {
		t.Fatal(err)
	}
	c.config.VerifyTimeout = 300 * time.Millisecond
	return c, r, s
}

// A user-owned source has no rollback, so a started target that fails a
// definite limit check gets a journaled limit stop before the latch. A
// failure that proves nothing gets no stop. A failed stop still latches,
// and the error says so.
func TestUserSwitchLimitStop(t *testing.T) {
	drift := fmt.Errorf("%w: %w", gpuruntime.ErrLimitCheckFailed, gpuruntime.ErrLimitDrift)
	stopErr := errors.New("stop refused")
	for name, c2 := range map[string]struct {
		healthErr error
		stopErr   error
		stops     int
		events    []string
		want      error
	}{
		"definite failure": {drift, nil, 1, []string{"intent/", "observation/ok"}, gpuruntime.ErrLimitDrift},
		"proves nothing":   {gpuruntime.ErrLimitReadback, nil, 0, nil, ErrVerifyTimeout},
		"stop fails":       {drift, stopErr, 1, []string{"intent/", "observation/failed"}, stopErr},
	} {
		c, r, s := userOwnedText(t)
		r.healthErr, r.visionStopErr = c2.healthErr, c2.stopErr
		_, err := c.SwitchUser(context.Background(), "vision", "test")
		events := limitStopEvents(t, r)
		if !errors.Is(err, c2.want) || stopsAfterStart(r) != c2.stops || !slices.Equal(events, c2.events) {
			t.Errorf("%s: user switch = %v; %d stops, events %v; calls %v", name, err, stopsAfterStart(r), events, r.calls)
		}
		if state, err := s.State(context.Background()); err != nil || state.Health != control.HealthError || state.Owner != control.OwnerUser {
			t.Errorf("%s: state after the failure = %+v, %v", name, state, err)
		}
	}
}

// When the caller cancels during a probe that finds a definite limit
// failure, the failure still reaches the transition, and the stop runs on
// its own context.
func TestUserSwitchLimitStopAfterCancel(t *testing.T) {
	c, r, _ := userOwnedText(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.healthErr = fmt.Errorf("%w: %w", gpuruntime.ErrLimitCheckFailed, gpuruntime.ErrLimitDrift)
	r.onHealth = cancel
	_, err := c.SwitchUser(ctx, "vision", "test")
	if !errors.Is(err, gpuruntime.ErrLimitCheckFailed) || !errors.Is(err, context.Canceled) || stopsAfterStart(r) != 1 {
		t.Fatalf("cancelled user switch = %v; calls %v", err, r.calls)
	}
	if last := r.stopCtxErrs[len(r.stopCtxErrs)-1]; last != nil {
		t.Fatalf("limit stop ran on a spent context: %v", last)
	}
}

// From a supervisor-owned source, the rollback alone stops the target.
func TestSupervisorSwitchStopsTargetOnce(t *testing.T) {
	c, r, _ := limitedController(t, limitedCatalog())
	ctx := context.Background()
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	c.config.VerifyTimeout = 2 * time.Second
	r.healthErr = fmt.Errorf("%w: %w", gpuruntime.ErrLimitCheckFailed, gpuruntime.ErrLimitDrift)
	_, err := c.Switch(ctx, "vision", "test")
	if !errors.Is(err, gpuruntime.ErrLimitDrift) || stopsAfterStart(r) != 1 || len(limitStopEvents(t, r)) != 0 {
		t.Fatalf("switch = %v; calls %v", err, r.calls)
	}
}

// Reconcile stops a retained unit with limits that fails a definite check
// in its readiness probe, on the fast path and on the full path.
func TestReconcileStopsRetainedUnitThatFailsReadiness(t *testing.T) {
	drift := fmt.Errorf("%w: %w", gpuruntime.ErrLimitCheckFailed, gpuruntime.ErrLimitDrift)
	for _, fast := range []bool{true, false} {
		catalog := limitedCatalog()
		catalog.Profiles[2].BootPolicy = "retain"
		c, r, s := limitedController(t, catalog)
		ctx := context.Background()
		r.fakeRuntime.active = control.WorkloadIdle
		r.visionActive = true
		c.config.VerifyTimeout = 2 * time.Second
		if fast {
			if state, err := c.Reconcile(ctx); err != nil || state.ActiveWorkload != "vision" {
				t.Fatalf("first reconcile = %+v, %v", state, err)
			}
		}
		r.calls = nil
		r.healthErr = drift
		_, err := c.Reconcile(ctx)
		if !errors.Is(err, gpuruntime.ErrLimitDrift) || r.visionActive || !slices.Contains(r.calls, "stop vision") {
			t.Errorf("fast %v: reconcile = %v; calls %v", fast, err, r.calls)
		}
		if state, err := s.State(ctx); err != nil || state.Health != control.HealthError {
			t.Errorf("fast %v: state = %+v, %v", fast, state, err)
		}
	}
}
