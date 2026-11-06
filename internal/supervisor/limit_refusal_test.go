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

// openRoute is the refusal of an open escape route. The runtime text names
// the route and its remedy, so the supervisor adds only recovery steps.
var openRoute = fmt.Errorf("%w through cron: /etc/cron.deny does not deny alice; close each route, or remove the limits through setup", gpuruntime.ErrEscapeRoute)

// routeRemedy ends the text of openRoute.
const routeRemedy = "; close each route, or remove the limits through setup"

// From Idle, an open route refusal needs no next steps: its text has the
// remedy.
func TestOpenRouteRefusalFromIdle(t *testing.T) {
	ctx := context.Background()
	c, r, _ := limitedController(t, limitedCatalog())
	r.fakeRuntime.active = control.WorkloadIdle
	if state, err := c.Reconcile(ctx); err != nil || state.ActiveWorkload != control.WorkloadIdle {
		t.Fatalf("reconcile to Idle = %+v, %v", state, err)
	}
	r.launchErr = openRoute
	_, err := c.Switch(ctx, "vision", "test")
	if !errors.Is(err, ErrLimitedTarget) || !strings.HasSuffix(err.Error(), routeRemedy) || strings.Contains(err.Error(), "next steps") {
		t.Fatalf("switch with an open route from Idle = %v", err)
	}
}

// When the refusal cannot read the current state, it assumes a latched
// state, so the next steps start with recover.
func TestOpenRouteRefusalWithUnreadableState(t *testing.T) {
	ctx := context.Background()
	c, r, _ := limitedController(t, limitedCatalog())
	r.launchErr = openRoute
	c.store = failingStore{storeGateway: c.store, stateErr: errors.New("database locked")}
	err := c.refuseLimitedTarget(ctx, "vision")
	if !errors.Is(err, ErrLimitedTarget) || !strings.HasSuffix(err.Error(), routeRemedy+"; next steps: first, run `gpu-mode recover`") {
		t.Fatalf("open route refusal with an unreadable state = %v", err)
	}
}

// With several rejected units, the next steps go on their own line, so they
// apply to each unit and not only to the last one.
func TestLimitRemedyOfSeveralUnits(t *testing.T) {
	one := fmt.Errorf("%w: vision: %w", ErrLimitedUnitRejected, openRoute)
	two := fmt.Errorf("%w: audio: %w", ErrLimitedUnitRejected, openRoute)
	err := limitRemedy(errors.Join(errors.Join(nil, one), two), remedyState{latched: true, kept: control.WorkloadIdle})
	if want := two.Error() + "\nnext steps: first, run `gpu-mode recover`"; !strings.HasSuffix(err.Error(), want) {
		t.Fatalf("remedy of two units = %q", err)
	}
	if err := limitRemedy(errors.Join(nil, one), remedyState{latched: true, kept: control.WorkloadIdle}); !strings.HasSuffix(err.Error(), routeRemedy+"; next steps: first, run `gpu-mode recover`") {
		t.Fatalf("remedy of one unit = %q", err)
	}
}

// seenState gives the refusal a fixed state to read.
type seenState struct {
	storeGateway
	state control.State
}

func (s seenState) State(context.Context) (control.State, error) { return s.state, nil }

// An open route refusal gives the gpu-mode commands that work for the
// state: recover first for a latched state, also a healthy one in a
// transition, and the user commands for a user-owned state. A state that
// needs no recovery gets no next steps.
func TestOpenRouteRefusalStepsFollowTheState(t *testing.T) {
	for name, c := range map[string]struct {
		state control.State
		want  string
	}{
		"healthy transition": {control.State{Owner: control.OwnerSupervisor, ActiveWorkload: control.WorkloadIdle, Phase: control.PhaseDraining, Health: control.HealthHealthy},
			routeRemedy + "; next steps: first, run `gpu-mode recover`"},
		"user owned": {control.State{Owner: control.OwnerUser, ActiveWorkload: control.WorkloadText, Phase: control.PhaseStable, Health: control.HealthHealthy},
			routeRemedy},
		"user owned and latched": {control.State{Owner: control.OwnerUser, ActiveWorkload: control.WorkloadText, Phase: control.PhaseReconciling, Health: control.HealthError},
			"; next steps: first, run `gpu-mode -target text recover-user`"},
		"user owned idle, latched": {control.State{Owner: control.OwnerUser, ActiveWorkload: control.WorkloadIdle, Phase: control.PhaseReconciling, Health: control.HealthError},
			"; next steps: first, run `gpu-mode -target idle recover-user`"},
		"user owned idle": {control.State{Owner: control.OwnerUser, ActiveWorkload: control.WorkloadIdle, Phase: control.PhaseStable, Health: control.HealthHealthy},
			routeRemedy},
		"user owned, unknown workload": {control.State{Owner: control.OwnerUser, ActiveWorkload: control.WorkloadUnknown, Phase: control.PhaseReconciling, Health: control.HealthError},
			"; next steps: first, stop the running workload unit outside gpu-mode, then run `gpu-mode -target idle recover-user`"},
		"supervisor owned limited unit, latched": {control.State{Owner: control.OwnerSupervisor, ActiveWorkload: "vision", Phase: control.PhaseReconciling, Health: control.HealthError},
			routeRemedy + "; next steps: first, run `gpu-mode recover`"},
		// The failed checks also refuse recover-user for the limited unit
		// itself, so it stops outside gpu-mode first.
		"user owned limited unit, latched": {control.State{Owner: control.OwnerUser, ActiveWorkload: "vision", Phase: control.PhaseReconciling, Health: control.HealthError},
			"; next steps: first, run `systemctl --user stop gws-owned-vision.service`, then run `gpu-mode -target idle recover-user`"},
	} {
		c2, r, _ := limitedController(t, limitedCatalog())
		r.launchErr = openRoute
		c2.store = seenState{storeGateway: c2.store, state: c.state}
		if err := c2.refuseLimitedTarget(context.Background(), "vision"); !errors.Is(err, ErrLimitedTarget) || !strings.HasSuffix(err.Error(), c.want) {
			t.Errorf("%s: open route refusal = %v", name, err)
		}
	}
}

// A user-owned switch with an open route needs no recovery, so the error
// ends with the remedy of the route.
func TestUserSwitchOpenRouteRefusal(t *testing.T) {
	ctx := context.Background()
	c, r, _ := limitedController(t, limitedCatalog())
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.TransferToUser(ctx, control.WorkloadText, "test"); err != nil {
		t.Fatal(err)
	}
	r.launchErr = openRoute
	_, err := c.SwitchUser(ctx, "vision", "test")
	if !errors.Is(err, ErrLimitedTarget) || !strings.HasSuffix(err.Error(), routeRemedy) {
		t.Fatalf("user switch with an open route = %v", err)
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
	r.protectionErr = fmt.Errorf("%w: %w", gpuruntime.ErrLimitCheckFailed, openRoute)
	stopErr := errors.New("stop refused")
	r.visionStopErr = stopErr
	_, err := c.Recover(ctx)
	if !errors.Is(err, ErrLimitedUnitRejected) || !errors.Is(err, stopErr) ||
		!strings.Contains(err.Error(), routeRemedy+"; next steps: first, run `gpu-mode recover`") {
		t.Fatalf("recover with a failed stop after a rejection = %v", err)
	}
	if state, err := s.State(ctx); err != nil || state.Health != control.HealthError {
		t.Fatalf("state after a failed recover = %+v, %v", state, err)
	}
}

// Each cause carries its own remedy, so recovery of a latched state comes
// first.
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
	r.protectionErr = fmt.Errorf("%w: %w", gpuruntime.ErrLimitCheckFailed, openRoute)
	_, err := c.Reconcile(ctx)
	if !errors.Is(err, ErrLimitedUnitRejected) || !errors.Is(err, gpuruntime.ErrLimitCheckFailed) || strings.Contains(err.Error(), gpuruntime.ErrLimitCheckFailed.Error()) ||
		!strings.Contains(err.Error(), "failed a check: vision (gws-owned-vision.service): programs of this account can run outside host protection through cron") {
		t.Fatalf("reconcile rejection = %v", err)
	}
}
