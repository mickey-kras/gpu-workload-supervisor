package supervisor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

// Each limit probe runs within ActionTimeout, so a hung command cannot
// block a switch, reconcile or recover.
func TestLimitProbesUseActionTimeout(t *testing.T) {
	ctx := context.Background()
	catalog := limitedCatalog()
	catalog.Profiles[2].BootPolicy = "retain"
	c, r, _ := limitedController(t, catalog)
	c.config.ActionTimeout = 50 * time.Millisecond
	r.fakeRuntime.active = control.WorkloadIdle
	r.visionActive = true
	r.protectionErr = gpuruntime.ErrLimitDrift
	start := time.Now()
	// Each path probes at least once and fails with its own error.
	for _, call := range []struct {
		name string
		run  func() error
		want error
	}{
		{"reconcile", func() error { _, err := c.Reconcile(ctx); return err }, ErrLimitedUnitRejected},
		{"recover", func() error { r.visionActive = true; _, err := c.Recover(ctx); return err }, ErrLimitedUnitRejected},
		{"switch", func() error {
			r.launchErr = gpuruntime.ErrHostCap
			_, err := c.Switch(ctx, "vision", "test")
			return err
		}, ErrLimitedTarget},
	} {
		probes := len(r.probeDeadlines)
		if err := call.run(); !errors.Is(err, call.want) || len(r.probeDeadlines) <= probes {
			t.Fatalf("%s = %v after %d probes, want %v and a probe", call.name, err, len(r.probeDeadlines)-probes, call.want)
		}
	}
	for i, deadline := range r.probeDeadlines {
		if deadline.IsZero() || deadline.Before(start) || deadline.After(time.Now().Add(c.config.ActionTimeout)) {
			t.Errorf("probe %d deadline = %v, start %v", i, deadline, start)
		}
	}
}

// From Idle, the gate refusal needs no switch first.
func TestGateRefusalFromIdle(t *testing.T) {
	ctx := context.Background()
	c, r, _ := limitedController(t, limitedCatalog())
	r.fakeRuntime.active = control.WorkloadIdle
	if state, err := c.Reconcile(ctx); err != nil || state.ActiveWorkload != control.WorkloadIdle {
		t.Fatalf("reconcile to Idle = %+v, %v", state, err)
	}
	r.launchErr = gpuruntime.ErrEscapeRoutesUnchecked
	_, err := c.Switch(ctx, "vision", "test")
	if !errors.Is(err, ErrLimitedTarget) || !strings.HasSuffix(err.Error(), "; next steps: remove the limits through setup") || strings.Contains(err.Error(), "Idle") {
		t.Fatalf("switch to a gated unit from Idle = %v", err)
	}
}

// When the refusal cannot read the current state, it assumes a latched
// state with a workload other than Idle, so the next steps start with
// recover and the switch to Idle.
func TestGateRefusalWithUnreadableState(t *testing.T) {
	ctx := context.Background()
	c, r, _ := limitedController(t, limitedCatalog())
	r.launchErr = gpuruntime.ErrEscapeRoutesUnchecked
	c.store = failingStore{storeGateway: c.store, stateErr: errors.New("database locked")}
	err := c.refuseLimitedTarget(ctx, "vision")
	if !errors.Is(err, ErrLimitedTarget) || !strings.HasSuffix(err.Error(), "; next steps: run `gpu-mode recover`, if a workload other than Idle runs then, run `gpu-mode -target idle switch`, then remove the limits through setup") {
		t.Fatalf("gate refusal with an unreadable state = %v", err)
	}
}

// With several rejected units, the next steps go on their own line, so they
// apply to each unit and not only to the last one.
func TestLimitRemedyOfSeveralUnits(t *testing.T) {
	one := fmt.Errorf("%w: vision: %w", ErrLimitedUnitRejected, gpuruntime.ErrEscapeRoutesUnchecked)
	two := fmt.Errorf("%w: audio: %w", ErrLimitedUnitRejected, gpuruntime.ErrEscapeRoutesUnchecked)
	err := limitRemedy(errors.Join(errors.Join(nil, one), two), remedyState{latched: true, kept: control.WorkloadIdle})
	if want := two.Error() + "\nnext steps: run `gpu-mode recover`, then remove the limits through setup"; !strings.HasSuffix(err.Error(), want) {
		t.Fatalf("remedy of two units = %q", err)
	}
	if err := limitRemedy(errors.Join(nil, one), remedyState{latched: true, kept: control.WorkloadIdle}); !strings.HasSuffix(err.Error(), "; next steps: run `gpu-mode recover`, then remove the limits through setup") {
		t.Fatalf("remedy of one unit = %q", err)
	}
}

// seenState gives the refusal a fixed state to read.
type seenState struct {
	storeGateway
	state control.State
}

func (s seenState) State(context.Context) (control.State, error) { return s.state, nil }

// The gate refusal gives the gpu-mode commands that work for the state:
// recover first for a latched state, also a healthy one in a transition,
// and the user commands for a user-owned state.
func TestGateRefusalStepsFollowTheState(t *testing.T) {
	for name, c := range map[string]struct {
		state control.State
		want  string
	}{
		"healthy transition": {control.State{Owner: control.OwnerSupervisor, ActiveWorkload: control.WorkloadIdle, Phase: control.PhaseDraining, Health: control.HealthHealthy},
			"; next steps: run `gpu-mode recover`, then remove the limits through setup"},
		"user owned": {control.State{Owner: control.OwnerUser, ActiveWorkload: control.WorkloadText, Phase: control.PhaseStable, Health: control.HealthHealthy},
			"; next steps: run `gpu-mode -target idle user-switch`, then remove the limits through setup"},
		"user owned and latched": {control.State{Owner: control.OwnerUser, ActiveWorkload: control.WorkloadText, Phase: control.PhaseReconciling, Health: control.HealthError},
			"; next steps: run `gpu-mode -target text recover-user`, if a workload other than Idle runs then, run `gpu-mode -target idle user-switch`, then remove the limits through setup"},
		"user owned idle, latched": {control.State{Owner: control.OwnerUser, ActiveWorkload: control.WorkloadIdle, Phase: control.PhaseReconciling, Health: control.HealthError},
			"; next steps: run `gpu-mode -target idle recover-user`, then remove the limits through setup"},
		"user owned idle": {control.State{Owner: control.OwnerUser, ActiveWorkload: control.WorkloadIdle, Phase: control.PhaseStable, Health: control.HealthHealthy},
			"resource limits; next steps: remove the limits through setup"},
		"user owned, unknown workload": {control.State{Owner: control.OwnerUser, ActiveWorkload: control.WorkloadUnknown, Phase: control.PhaseReconciling, Health: control.HealthError},
			"; next steps: stop the running workload unit outside gpu-mode, run `gpu-mode -target idle recover-user`, then remove the limits through setup"},
		// Recover stops a kept unit with limits, so no switch to Idle
		// follows.
		"supervisor owned gated unit, latched": {control.State{Owner: control.OwnerSupervisor, ActiveWorkload: "vision", Phase: control.PhaseReconciling, Health: control.HealthError},
			"resource limits; next steps: run `gpu-mode recover`, then remove the limits through setup"},
		// The gate also refuses recover-user for the gated unit itself.
		"user owned gated unit, latched": {control.State{Owner: control.OwnerUser, ActiveWorkload: "vision", Phase: control.PhaseReconciling, Health: control.HealthError},
			"; next steps: run `systemctl --user stop gws-owned-vision.service`, run `gpu-mode -target idle recover-user`, then remove the limits through setup"},
	} {
		c2, r, _ := limitedController(t, limitedCatalog())
		r.launchErr = gpuruntime.ErrEscapeRoutesUnchecked
		c2.store = seenState{storeGateway: c2.store, state: c.state}
		if err := c2.refuseLimitedTarget(context.Background(), "vision"); !errors.Is(err, ErrLimitedTarget) || !strings.HasSuffix(err.Error(), c.want) {
			t.Errorf("%s: gate refusal = %v", name, err)
		}
	}
}

// A user-owned switch to a gated unit gives the user-switch step, which
// keeps the user ownership.
func TestUserSwitchGateRefusal(t *testing.T) {
	ctx := context.Background()
	c, r, _ := limitedController(t, limitedCatalog())
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.TransferToUser(ctx, control.WorkloadText, "test"); err != nil {
		t.Fatal(err)
	}
	r.launchErr = gpuruntime.ErrEscapeRoutesUnchecked
	_, err := c.SwitchUser(ctx, "vision", "test")
	if !errors.Is(err, ErrLimitedTarget) || !strings.HasSuffix(err.Error(), "; next steps: run `gpu-mode -target idle user-switch`, then remove the limits through setup") {
		t.Fatalf("user switch to a gated unit = %v", err)
	}
}

// When recover rejects a unit and then cannot settle, the state stays
// latched, so the next steps start with recover again.
func TestFailedRecoverAfterRejectionNamesRecover(t *testing.T) {
	ctx := context.Background()
	catalog := limitedCatalog()
	catalog.Profiles[2].BootPolicy = "retain"
	c, r, s := limitedController(t, catalog)
	r.fakeRuntime.active = control.WorkloadIdle
	r.visionActive = true
	r.protectionErr = fmt.Errorf("%w: %w", gpuruntime.ErrLimitCheckFailed, gpuruntime.ErrEscapeRoutesUnchecked)
	stopErr := errors.New("stop refused")
	r.visionStopErr = stopErr
	_, err := c.Recover(ctx)
	if !errors.Is(err, ErrLimitedUnitRejected) || !errors.Is(err, stopErr) ||
		!strings.Contains(err.Error(), "; next steps: run `gpu-mode recover`, if a workload other than Idle runs then, run `gpu-mode -target idle switch`, then remove the limits through setup") {
		t.Fatalf("recover with a failed stop after a rejection = %v", err)
	}
	if state, err := s.State(ctx); err != nil || state.Health != control.HealthError {
		t.Fatalf("state after a failed recover = %+v, %v", state, err)
	}
}

// A cause other than the gate carries its own remedy, so recovery of a
// latched state comes first.
func TestRecoveryComesBeforeTheCauseRemedy(t *testing.T) {
	c, r, _ := limitedController(t, limitedCatalog())
	r.launchErr = fmt.Errorf("%w: use 4096 MiB or less for memoryMaxMiB, or remove the limits through setup", gpuruntime.ErrLimitReserve)
	c.store = seenState{storeGateway: c.store, state: control.State{Owner: control.OwnerSupervisor, ActiveWorkload: control.WorkloadIdle,
		Phase: control.PhaseReconciling, Health: control.HealthError}}
	err := c.refuseLimitedTarget(context.Background(), "vision")
	if !errors.Is(err, ErrLimitedTarget) || !strings.HasSuffix(err.Error(), "or remove the limits through setup; next steps: first, run `gpu-mode recover`") {
		t.Fatalf("latched reserve refusal = %v", err)
	}
}

// A rejection of a running unit names one class: the text leaves out the
// started-unit prefix, but the error keeps both classes.
func TestRejectionNamesOneClass(t *testing.T) {
	ctx := context.Background()
	catalog := limitedCatalog()
	catalog.Profiles[2].BootPolicy = "retain"
	c, r, _ := limitedController(t, catalog)
	r.fakeRuntime.active = control.WorkloadIdle
	r.visionActive = true
	r.protectionErr = fmt.Errorf("%w: %w", gpuruntime.ErrLimitCheckFailed, gpuruntime.ErrEscapeRoutesUnchecked)
	_, err := c.Reconcile(ctx)
	if !errors.Is(err, ErrLimitedUnitRejected) || !errors.Is(err, gpuruntime.ErrLimitCheckFailed) || strings.Contains(err.Error(), gpuruntime.ErrLimitCheckFailed.Error()) ||
		!strings.Contains(err.Error(), "failed a check: vision (gws-owned-vision.service): this release has no escape route checks") {
		t.Fatalf("reconcile rejection = %v", err)
	}
}
