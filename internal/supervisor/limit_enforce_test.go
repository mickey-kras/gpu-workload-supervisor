package supervisor

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

// stoppingRuntime adds the outcome and the release check of a limit stop
// to limitedRuntime.
type stoppingRuntime struct {
	*limitedRuntime
	releaseErr    error
	releaseChecks int
	released      []control.Workload
	// releaseErrs gives the results of the next checks, before releaseErr.
	releaseErrs []error
}

func (r *stoppingRuntime) LimitOutcome(context.Context, control.Workload) gpuruntime.LimitOutcome {
	return gpuruntime.LimitOutcome{Class: gpuruntime.LimitOutcomeKill, Detail: "memory.events oom_kill 1"}
}

func (r *stoppingRuntime) LimitStopReleased(_ context.Context, w control.Workload) error {
	r.releaseChecks++
	r.released = append(r.released, w)
	if len(r.releaseErrs) != 0 {
		err := r.releaseErrs[0]
		r.releaseErrs = r.releaseErrs[1:]
		return err
	}
	return r.releaseErr
}

// limitFixture is a supervisor-owned state latched while vision runs.
type limitFixture struct {
	c      *Controller
	r      *stoppingRuntime
	s      *store.Store
	offset time.Duration
}

var limitDrift = fmt.Errorf("%w: %w", gpuruntime.ErrLimitCheckFailed, gpuruntime.ErrLimitDrift)

// latchedVision switches to vision, admits work when work is set, and
// latches the state through a failed status check.
func latchedVision(t *testing.T, work bool) *limitFixture {
	t.Helper()
	c, lr, s := limitedController(t, limitedCatalog())
	f := &limitFixture{c: c, r: &stoppingRuntime{limitedRuntime: lr}, s: s}
	c.runtime = f.r
	base := time.Now()
	c.now = func() time.Time { return base.Add(f.offset) }
	ctx := context.Background()
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	state, err := c.Switch(ctx, "vision", "test")
	if err != nil {
		t.Fatal(err)
	}
	if work {
		if _, err := s.AdmitWorkToken(ctx, "job", "", "vision", state.LeaseFence); err != nil {
			t.Fatal(err)
		}
	}
	lr.healthErr = limitDrift
	if _, err := c.Status(ctx); !errors.Is(err, gpuruntime.ErrLimitDrift) {
		t.Fatalf("status latch = %v", err)
	}
	lr.healthErr = nil
	lr.protectionErr = limitDrift
	lr.calls = nil
	return f
}

func (f *limitFixture) state(t *testing.T) control.State {
	t.Helper()
	state, err := f.s.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func (f *limitFixture) record(t *testing.T) (store.LimitStop, bool) {
	t.Helper()
	stop, ok, err := f.s.LimitStop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return stop, ok
}

// stops lists the transitions of limit stops as status, then each event
// as kind/action/outcome.
func (f *limitFixture) stops(t *testing.T) [][]string {
	t.Helper()
	db, err := sql.Open("sqlite", f.r.statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT transition_id, status FROM transitions WHERE initiator = ? ORDER BY created_at, transition_id`, limitStopInitiator)
	if err != nil {
		t.Fatal(err)
	}
	type row struct{ id, status string }
	var list []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.status); err != nil {
			t.Fatal(err)
		}
		list = append(list, r)
	}
	rows.Close()
	var out [][]string
	for _, r := range list {
		events, err := transitionEvents(f.r.statePath, context.Background(), r.id)
		if err != nil {
			t.Fatal(err)
		}
		entry := []string{r.status}
		for _, e := range events {
			entry = append(entry, e.Kind+"/"+e.Action+"/"+e.Outcome)
		}
		out = append(out, entry)
	}
	return out
}

func (f *limitFixture) stopCount() int {
	n := 0
	for _, call := range f.r.calls {
		if call == "stop vision" {
			n++
		}
	}
	return n
}

// requireLatchedIdle checks the state after a passed limit stop: idle, and
// latched until recover.
func requireLatchedIdle(t *testing.T, f *limitFixture) {
	t.Helper()
	state := f.state(t)
	if state.ActiveWorkload != control.WorkloadIdle || state.DesiredWorkload != control.WorkloadIdle || state.Health != control.HealthError ||
		state.Admission != control.AdmissionClosed || state.Owner != control.OwnerSupervisor {
		t.Fatalf("state after the limit stop = %+v", state)
	}
	// The record stays until recover, so each tick still exits nonzero.
	if stop, ok := f.record(t); !ok || stop.StoppingAt.IsZero() {
		t.Fatalf("record after the idle record = %+v, %v", stop, ok)
	}
	if err := f.c.EnforceLimits(context.Background()); !errors.Is(err, ErrLimitStopped) || !strings.Contains(err.Error(), "holds the error state") {
		t.Fatalf("tick after the limit stop = %v", err)
	}
}

// A definite failure with no admitted work stops the unit at once through
// a journaled transition, checks the release and records idle. The tick
// exits nonzero, and so does each later tick until recover ends the latch
// and removes the record.
func TestLimitTickStopsLatchedUnit(t *testing.T) {
	f := latchedVision(t, false)
	var log bytes.Buffer
	err := f.c.LimitTick(context.Background(), &log)
	if !errors.Is(err, ErrLimitStopped) || !errors.Is(err, gpuruntime.ErrLimitDrift) || !strings.Contains(err.Error(), "limit outcome: kill") || log.Len() != 0 {
		t.Fatalf("limit tick = %v; log %q", err, log.String())
	}
	if f.stopCount() != 1 || f.r.releaseChecks != 1 || f.r.visionActive {
		t.Fatalf("calls %v; %d release checks", f.r.calls, f.r.releaseChecks)
	}
	requireLatchedIdle(t, f)
	want := [][]string{{"committed",
		"observation/limit stop reason vision/definite check failure; limit outcome: kill (memory.events oom_kill 1)",
		"intent/limit stop vision/", "observation/limit stop vision/ok",
		"observation/limit stop release check vision/ok"}}
	if got := f.stops(t); !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("journal = %q", got)
	}
	if err := f.c.LimitTick(context.Background(), &log); !errors.Is(err, ErrLimitStopped) || f.stopCount() != 1 {
		t.Fatalf("next tick = %v; calls %v", err, f.r.calls)
	}
	if _, err := f.c.Switch(context.Background(), "text", "test"); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("switch after the limit stop = %v", err)
	}
	if state, err := f.c.Recover(context.Background()); err != nil || state.Health != control.HealthHealthy {
		t.Fatalf("recover = %+v, %v", state, err)
	}
	if _, ok := f.record(t); ok {
		t.Fatal("the record outlives recover")
	}
	if err := f.c.LimitTick(context.Background(), &log); err != nil {
		t.Fatalf("tick after recover = %v", err)
	}
}

// No stop runs for a state that is not latched, for a user-owned state,
// for a check that passes, or for a catalog without limits. A stale
// record goes away.
func TestLimitTickNoStopCases(t *testing.T) {
	ctx := context.Background()
	for name, edit := range map[string]func(*testing.T, *limitFixture){
		"passing check": func(_ *testing.T, f *limitFixture) { f.r.protectionErr = nil },
		"healthy state": func(t *testing.T, f *limitFixture) {
			state := f.state(t)
			state.Health, state.Phase, state.Admission = control.HealthHealthy, control.PhaseStable, control.AdmissionOpen
			state.ActiveWorkload = "vision"
			if _, err := f.s.UpdateState(ctx, state.Version, state); err != nil {
				t.Fatal(err)
			}
		},
		"user owned": func(t *testing.T, f *limitFixture) {
			state := f.state(t)
			state.Owner = control.OwnerUser
			if _, err := f.s.UpdateState(ctx, state.Version, state); err != nil {
				t.Fatal(err)
			}
		},
		"no limits": func(_ *testing.T, f *limitFixture) {
			f.c.config.Catalog.Catalog = testCatalog()
		},
	} {
		f := latchedVision(t, false)
		edit(t, f)
		if _, err := f.s.MarkLimitFailure(ctx, "vision", false, time.Now()); err != nil {
			t.Fatal(err)
		}
		err := f.c.EnforceLimits(ctx)
		_, stored := f.record(t)
		if err != nil || f.stopCount() != 0 || len(f.stops(t)) != 0 || stored != (name == "passing check") {
			t.Errorf("%s: enforce = %v; calls %v; record %v", name, err, f.r.calls, stored)
		}
	}
}

// Admitted work of the unit defers the stop until the drain timeout after
// the first definite failure. Then the stop cuts the work off.
func TestLimitTickDrainsThenCutsOff(t *testing.T) {
	f := latchedVision(t, true)
	ctx := context.Background()
	var log bytes.Buffer
	if err := f.c.LimitTick(ctx, &log); err != nil || !strings.Contains(log.String(), "resolve-work") || f.stopCount() != 0 {
		t.Fatalf("tick with pending work = %v; log %q; calls %v", err, log.String(), f.r.calls)
	}
	stop, ok := f.record(t)
	if !ok || stop.DefiniteAt.IsZero() || !stop.StoppingAt.IsZero() {
		t.Fatalf("record = %+v, %v", stop, ok)
	}
	f.offset = f.c.config.DrainTimeout + time.Millisecond
	err := f.c.LimitTick(ctx, &log)
	if !errors.Is(err, ErrLimitStopped) || !strings.Contains(err.Error(), "cut off") || f.stopCount() != 1 {
		t.Fatalf("tick after the drain = %v; calls %v", err, f.r.calls)
	}
	requireLatchedIdle(t, f)
}

// A recorded failure whose step fails after drain plus verify exits
// nonzero but stops nothing.
func TestLimitTickEscalatesOverdueDeferral(t *testing.T) {
	f := latchedVision(t, true)
	ctx := context.Background()
	f.r.protectionErr = gpuruntime.ErrLimitReadback
	if err := f.c.EnforceLimits(ctx); !errors.Is(err, ErrLimitStopDeferred) {
		t.Fatalf("unproven failure = %v", err)
	}
	f.offset = f.c.config.DrainTimeout + f.c.config.VerifyTimeout + 100*time.Millisecond
	f.r.observeErr = errors.New("bus down")
	var log bytes.Buffer
	err := f.c.LimitTick(ctx, &log)
	if !errors.Is(err, ErrLimitStopOverdue) || errors.Is(err, ErrLimitStopDeferred) || f.stopCount() != 0 {
		t.Fatalf("overdue deferral = %v; calls %v", err, f.r.calls)
	}
}

// A failure that turns definite late, with pending work, stops no later
// than the overdue time of the first failure. A later check that proves
// nothing keeps the definite plan.
func TestLimitTickLateDefiniteFailureStopsByOverdueTime(t *testing.T) {
	f := latchedVision(t, true)
	ctx := context.Background()
	d, v := f.c.config.DrainTimeout, f.c.config.VerifyTimeout
	f.r.protectionErr = gpuruntime.ErrLimitReadback
	if err := f.c.EnforceLimits(ctx); !errors.Is(err, ErrLimitStopDeferred) {
		t.Fatalf("unproven failure = %v", err)
	}
	f.r.protectionErr = limitDrift
	f.offset = v + d/2
	if err := f.c.EnforceLimits(ctx); !errors.Is(err, ErrLimitStopDeferred) {
		t.Fatalf("definite failure with work = %v", err)
	}
	f.r.protectionErr = gpuruntime.ErrLimitReadback
	f.offset = d + v + 100*time.Millisecond
	err := f.c.EnforceLimits(ctx)
	if !errors.Is(err, ErrLimitStopped) || !strings.Contains(err.Error(), "recorded definite failure") ||
		!strings.Contains(err.Error(), "cut off") || f.stopCount() != 1 {
		t.Fatalf("late definite failure = %v; calls %v", err, f.r.calls)
	}
}

// Two failing units that take the record in turn keep the times of the
// latch, so the stop still comes once the first failure is overdue.
func TestLimitTickAlternatingUnitsKeepLatchTimes(t *testing.T) {
	f := twoLimitedUnits(t)
	ctx := context.Background()
	f.r.alsoActive = []control.Workload{"audio"}
	for i := range 4 {
		// vision fails only on even ticks, audio always.
		visionErr := error(nil)
		if i%2 == 0 {
			visionErr = gpuruntime.ErrLimitReadback
		}
		f.r.protectionErrs = map[control.Workload]error{"vision": visionErr, "audio": gpuruntime.ErrLimitReadback}
		f.offset = time.Duration(i) * f.c.config.DrainTimeout / 2
		if err := f.c.EnforceLimits(ctx); !errors.Is(err, ErrLimitStopDeferred) {
			t.Fatalf("tick %d = %v", i, err)
		}
	}
	f.offset = f.c.config.DrainTimeout + f.c.config.VerifyTimeout + time.Millisecond
	err := f.c.EnforceLimits(ctx)
	if !errors.Is(err, ErrLimitStopped) || !strings.Contains(err.Error(), "overdue check failure") {
		t.Fatalf("overdue tick = %v; calls %v", err, f.r.calls)
	}
}

// A failure that proves nothing defers until drain plus verify after the
// first failure. Then the system manager stops the unit, and the journal
// records the reason.
func TestLimitTickStopsOverdueUnprovenFailure(t *testing.T) {
	f := latchedVision(t, true)
	ctx := context.Background()
	f.r.protectionErr = gpuruntime.ErrLimitReadback
	if err := f.c.EnforceLimits(ctx); !errors.Is(err, ErrLimitStopDeferred) || f.stopCount() != 0 {
		t.Fatalf("unproven failure = %v", err)
	}
	f.offset = f.c.config.DrainTimeout + f.c.config.VerifyTimeout + time.Millisecond
	err := f.c.EnforceLimits(ctx)
	if !errors.Is(err, ErrLimitStopped) || !errors.Is(err, gpuruntime.ErrLimitReadback) || !strings.Contains(err.Error(), "cut off") || f.stopCount() != 1 {
		t.Fatalf("overdue unproven failure = %v; calls %v", err, f.r.calls)
	}
	requireLatchedIdle(t, f)
	if got := f.stops(t); len(got) != 1 || !strings.Contains(got[0][1], "overdue check failure that proves nothing") {
		t.Fatalf("journal = %q", got)
	}
}

// A failed stop fails the transition with an unknown workload and keeps
// the record. The next tick stops the unit again.
func TestLimitTickRetriesFailedStop(t *testing.T) {
	f := latchedVision(t, false)
	ctx := context.Background()
	f.r.visionStopErr = errors.New("stop refused")
	err := f.c.EnforceLimits(ctx)
	if !errors.Is(err, f.r.visionStopErr) || !errors.Is(err, ErrLimitStopFailed) || errors.Is(err, ErrLimitStopped) {
		t.Fatalf("failed stop = %v", err)
	}
	if state := f.state(t); state.ActiveWorkload != control.WorkloadUnknown || state.Health != control.HealthError {
		t.Fatalf("state after a failed stop = %+v", state)
	}
	if stop, ok := f.record(t); !ok || stop.StoppingAt.IsZero() {
		t.Fatalf("record after a failed stop = %+v, %v", stop, ok)
	}
	f.r.visionStopErr = nil
	if err := f.c.EnforceLimits(ctx); !errors.Is(err, ErrLimitStopped) || f.stopCount() != 2 {
		t.Fatalf("retried stop = %v; calls %v", err, f.r.calls)
	}
	requireLatchedIdle(t, f)
	got := f.stops(t)
	if len(got) != 2 || got[0][0] != "failed" || got[0][3] != "observation/limit stop vision/failed" || got[1][0] != "committed" {
		t.Fatalf("journal = %q", got)
	}
}

// A failed release check leaves an unknown workload. The next tick finds
// the unit stopped, checks the release again and records idle.
func TestLimitTickFinishesEarlierStop(t *testing.T) {
	f := latchedVision(t, false)
	ctx := context.Background()
	f.c.config.VerifyTimeout = 150 * time.Millisecond
	f.r.releaseErr = errors.New("cgroup still populated")
	err := f.c.EnforceLimits(ctx)
	if !errors.Is(err, f.r.releaseErr) || !errors.Is(err, ErrLimitStopped) || f.r.releaseChecks < 2 || f.r.visionActive {
		t.Fatalf("failed release = %v after %d checks", err, f.r.releaseChecks)
	}
	if state := f.state(t); state.ActiveWorkload != control.WorkloadUnknown {
		t.Fatalf("state after a failed release = %+v", state)
	}
	checks := f.r.releaseChecks
	err = f.c.EnforceLimits(ctx)
	if !errors.Is(err, ErrLimitStopFailed) || errors.Is(err, ErrLimitStopped) || f.r.releaseChecks != checks+1 || f.stopCount() != 1 {
		t.Fatalf("failed retry = %v after %d checks; calls %v", err, f.r.releaseChecks-checks, f.r.calls)
	}
	f.r.releaseErr = nil
	err = f.c.EnforceLimits(ctx)
	if !errors.Is(err, ErrLimitStopped) || !strings.Contains(err.Error(), "finished the earlier limit stop") {
		t.Fatalf("finished stop = %v", err)
	}
	requireLatchedIdle(t, f)
	got := f.stops(t)
	// The failed retry probes once and starts no transition.
	if len(got) != 2 || got[0][0] != "failed" || got[0][len(got[0])-1] != "observation/limit stop release check vision/timeout" ||
		got[1][0] != "committed" || got[1][1] != "observation/limit stop reason vision/finish an earlier limit stop" {
		t.Fatalf("journal = %q", got)
	}
}

// A failed binding check in the release check ends it at once, also on
// the retry of a later tick, so no tick polls for VerifyTimeout.
func TestLimitStopChecksChangedBindingOnce(t *testing.T) {
	f := latchedVision(t, false)
	f.c.config.VerifyTimeout = time.Minute
	f.r.protectionErr = gpuruntime.ErrLaunchChanged
	f.r.releaseErr = gpuruntime.ErrLaunchChanged
	err := f.c.EnforceLimits(context.Background())
	if !errors.Is(err, ErrLimitStopped) || !strings.Contains(err.Error(), "the launch binding changed") || f.r.releaseChecks != 1 {
		t.Fatalf("changed binding = %v after %d checks", err, f.r.releaseChecks)
	}
	err = f.c.EnforceLimits(context.Background())
	if !errors.Is(err, ErrLimitStopFailed) || !strings.Contains(err.Error(), "the launch binding changed") || f.r.releaseChecks != 2 {
		t.Fatalf("retry after the binding = %v after %d checks", err, f.r.releaseChecks)
	}
	f.r.releaseErr = nil
	f.r.protectionErr = gpuruntime.ErrLaunchChanged
	if err := f.c.EnforceLimits(context.Background()); !errors.Is(err, ErrLimitStopped) {
		t.Fatalf("finish after the binding = %v", err)
	}
	requireLatchedIdle(t, f)
}

// A runtime without the release check of a limit stop never records idle.
func TestLimitStopWithoutReleaseCheckKeepsUnknownWorkload(t *testing.T) {
	f := latchedVision(t, false)
	f.c.runtime = f.r.limitedRuntime
	err := f.c.EnforceLimits(context.Background())
	if !errors.Is(err, gpuruntime.ErrLimitUnchecked) || !strings.Contains(err.Error(), "the runtime cannot read it") || f.stopCount() != 1 {
		t.Fatalf("stop without the release check = %v", err)
	}
	if state := f.state(t); state.ActiveWorkload != control.WorkloadUnknown {
		t.Fatalf("state = %+v", state)
	}
}

// A record with no started stop goes away when its unit stopped without a
// limit stop or left the catalog. A started stop of a unit that left the
// catalog stays, and the tick exits nonzero.
func TestLimitTickClearsSettledRecord(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		w        control.Workload
		stopping bool
	}{{"stopped elsewhere", "vision", false}, {"unconfigured", "other", false}, {"unconfigured started", "other", true}} {
		f := latchedVision(t, false)
		f.r.visionActive = false
		if _, err := f.s.MarkLimitFailure(ctx, tc.w, true, time.Now()); err != nil {
			t.Fatal(err)
		}
		if tc.stopping {
			if err := f.s.MarkLimitStopping(ctx, tc.w, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		err := f.c.EnforceLimits(ctx)
		_, ok := f.record(t)
		if tc.stopping {
			if !errors.Is(err, ErrLimitStopFailed) || !strings.Contains(err.Error(), "left the catalog") || !ok || f.stopCount() != 0 {
				t.Errorf("%s: enforce = %v; record %v; calls %v", tc.name, err, ok, f.r.calls)
			}
			continue
		}
		if err != nil || ok || f.stopCount() != 0 {
			t.Errorf("%s: enforce = %v; record %v; calls %v", tc.name, err, ok, f.r.calls)
		}
	}
}

// One passing check of a running unit keeps the first failure times.
func TestLimitTickKeepsRecordOfRunningUnit(t *testing.T) {
	f := latchedVision(t, true)
	ctx := context.Background()
	if err := f.c.EnforceLimits(ctx); !errors.Is(err, ErrLimitStopDeferred) {
		t.Fatalf("deferral = %v", err)
	}
	before, _ := f.record(t)
	f.r.protectionErr = nil
	if err := f.c.EnforceLimits(ctx); err != nil {
		t.Fatalf("passing check = %v", err)
	}
	if after, ok := f.record(t); !ok || after != before {
		t.Fatalf("record = %+v, %v; want %+v", after, ok, before)
	}
}

// limitStore fails chosen store calls of the limit stop.
type limitStore struct {
	storeGateway
	recordErr, stateErr, runningErr, markErr, pendingErr, stoppingErr, startErr, eventErr, finishErr error
	// eventAction is the action prefix that eventErr fails; empty means
	// the reason observation.
	eventAction string
	phaseErrs   map[control.Phase]error
	running     string
}

func (s *limitStore) LimitStop(ctx context.Context) (store.LimitStop, bool, error) {
	if s.recordErr != nil {
		return store.LimitStop{}, false, s.recordErr
	}
	return s.storeGateway.LimitStop(ctx)
}

func (s *limitStore) State(ctx context.Context) (control.State, error) {
	if s.stateErr != nil {
		return control.State{}, s.stateErr
	}
	return s.storeGateway.State(ctx)
}

func (s *limitStore) InProgressTransition(ctx context.Context) (string, error) {
	if s.runningErr != nil || s.running != "" {
		return s.running, s.runningErr
	}
	return s.storeGateway.InProgressTransition(ctx)
}

func (s *limitStore) MarkLimitFailure(ctx context.Context, w control.Workload, definite bool, at time.Time) (store.LimitStop, error) {
	if s.markErr != nil {
		return store.LimitStop{}, s.markErr
	}
	return s.storeGateway.MarkLimitFailure(ctx, w, definite, at)
}

func (s *limitStore) PendingWorkFor(ctx context.Context, w control.Workload) (int, error) {
	if s.pendingErr != nil {
		return 0, s.pendingErr
	}
	return s.storeGateway.PendingWorkFor(ctx, w)
}

func (s *limitStore) MarkLimitStopping(ctx context.Context, w control.Workload, at time.Time) error {
	if s.stoppingErr != nil {
		return s.stoppingErr
	}
	return s.storeGateway.MarkLimitStopping(ctx, w, at)
}

func (s *limitStore) SetTransitionPhase(ctx context.Context, id string, version uint64, phase control.Phase) (control.State, error) {
	if err := s.phaseErrs[phase]; err != nil {
		return control.State{}, err
	}
	return s.storeGateway.SetTransitionPhase(ctx, id, version, phase)
}

func (s *limitStore) StartTransition(ctx context.Context, version uint64, tr store.Transition) (control.State, error) {
	if s.startErr != nil {
		return control.State{}, s.startErr
	}
	return s.storeGateway.StartTransition(ctx, version, tr)
}

func (s *limitStore) AppendTransitionEvent(ctx context.Context, e store.TransitionEvent) error {
	prefix := s.eventAction
	if prefix == "" {
		prefix = "limit stop reason"
	}
	if s.eventErr != nil && strings.HasPrefix(e.Action, prefix) {
		return s.eventErr
	}
	return s.storeGateway.AppendTransitionEvent(ctx, e)
}

func (s *limitStore) FinishTransition(ctx context.Context, id, status string, version uint64, final control.State) (control.State, error) {
	if s.finishErr != nil {
		return control.State{}, s.finishErr
	}
	return s.storeGateway.FinishTransition(ctx, id, status, version, final)
}

// A failed store call defers the stop, or fails it once the stop began.
// Without a stored failure, a failed step before the check is a deferral
// that never escalates.
func TestLimitTickStoreFailures(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("store failed")
	for name, tc := range map[string]struct {
		edit   func(*limitStore)
		stored bool
		want   error
		stops  int
	}{
		"state without record": {func(s *limitStore) { s.stateErr = boom }, false, boom, 0},
		"state with record":    {func(s *limitStore) { s.stateErr = boom }, true, ErrLimitStopDeferred, 0},
		"record read":          {func(s *limitStore) { s.recordErr = boom }, false, ErrLimitStopFailed, 0},
		"transition check":     {func(s *limitStore) { s.runningErr = boom }, false, ErrLimitCheckSkipped, 0},
		"running transition":   {func(s *limitStore) { s.running = "t1" }, true, ErrTransitionRunning, 0},
		"failure record":       {func(s *limitStore) { s.markErr = boom }, false, ErrLimitedUnitRejected, 0},
		"pending work":         {func(s *limitStore) { s.pendingErr = boom }, false, ErrLimitStopDeferred, 0},
		"stopping record":      {func(s *limitStore) { s.stoppingErr = boom }, false, ErrLimitStopFailed, 0},
		"transition start":     {func(s *limitStore) { s.startErr = boom }, false, ErrLimitStopFailed, 0},
		"reason journal":       {func(s *limitStore) { s.eventErr = boom }, false, ErrLimitStopFailed, 0},
		"transition finish":    {func(s *limitStore) { s.finishErr = boom }, false, ErrLimitStopped, 1},
		"unloading phase":      {func(s *limitStore) { s.phaseErrs = map[control.Phase]error{control.PhaseUnloading: boom} }, false, ErrLimitStopFailed, 0},
		"verifying phase":      {func(s *limitStore) { s.phaseErrs = map[control.Phase]error{control.PhaseVerifying: boom} }, false, ErrLimitStopped, 1},
	} {
		f := latchedVision(t, false)
		s := &limitStore{storeGateway: f.s}
		if tc.stored {
			if _, err := f.s.MarkLimitFailure(ctx, "vision", false, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		tc.edit(s)
		f.c.store = s
		err := f.c.EnforceLimits(ctx)
		quiet := tc.want == ErrLimitStopDeferred || tc.want == ErrLimitCheckSkipped || name == "running transition"
		if !errors.Is(err, tc.want) || f.stopCount() != tc.stops || (!quiet && !errors.Is(err, boom)) ||
			(tc.stops == 0 && errors.Is(err, ErrLimitStopped)) {
			t.Errorf("%s: enforce = %v; calls %v", name, err, f.r.calls)
		}
		var log bytes.Buffer
		if errors.Is(err, ErrLimitStopDeferred) || errors.Is(err, ErrLimitCheckSkipped) {
			if tickErr := f.c.LimitTick(ctx, &log); tickErr != nil || log.Len() == 0 {
				t.Errorf("%s: deferred tick = %v; log %q", name, tickErr, log.String())
			}
		}
	}
}

// A failed record read fails the tick while the state is latched, since
// the record can hold a started stop. Without the latch, it defers.
func TestLimitTickRecordReadFailure(t *testing.T) {
	ctx := context.Background()
	f := latchedVision(t, false)
	f.c.store = &limitStore{storeGateway: f.s, recordErr: errors.New("record unreadable")}
	if err := f.c.EnforceLimits(ctx); !errors.Is(err, ErrLimitStopFailed) || f.stopCount() != 0 {
		t.Fatalf("latched record read = %v", err)
	}
	f.c.store = &limitStore{storeGateway: f.s, recordErr: errors.New("record unreadable"), stateErr: errors.New("state unreadable")}
	if err := f.c.EnforceLimits(ctx); err == nil || errors.Is(err, ErrLimitStopDeferred) {
		t.Fatalf("unreadable record and state = %v", err)
	}
	state := f.state(t)
	state.Health, state.Phase, state.Admission, state.ActiveWorkload = control.HealthHealthy, control.PhaseStable, control.AdmissionOpen, "vision"
	if _, err := f.s.UpdateState(ctx, state.Version, state); err != nil {
		t.Fatal(err)
	}
	f.c.store = &limitStore{storeGateway: f.s, recordErr: errors.New("record unreadable")}
	if err := f.c.EnforceLimits(ctx); !errors.Is(err, ErrLimitStopDeferred) {
		t.Fatalf("record read of a healthy state = %v", err)
	}
}

// With no stored failure, a failed observation or catalog check skips the
// limit check: no stop is pending, and the tick exits zero.
func TestLimitTickRuntimeFailures(t *testing.T) {
	ctx := context.Background()
	f := latchedVision(t, false)
	f.r.observeErr = errors.New("bus down")
	err := f.c.EnforceLimits(ctx)
	if !errors.Is(err, ErrLimitCheckSkipped) || errors.Is(err, ErrLimitStopDeferred) || !errors.Is(err, f.r.observeErr) {
		t.Fatalf("observation failure = %v", err)
	}
	var log bytes.Buffer
	if err := f.c.LimitTick(ctx, &log); err != nil || !strings.Contains(log.String(), "limit check skipped") {
		t.Fatalf("tick = %v; log %q", err, log.String())
	}
	f.r.observeErr = nil
	f.c.config.Catalog.Revision = "other"
	if err := f.c.EnforceLimits(ctx); !errors.Is(err, ErrLimitCheckSkipped) {
		t.Fatalf("catalog failure = %v", err)
	}
}

// A state write that ends the latch removes the record, so a new latch
// gets new times and a new drain.
func TestLimitTickNewLatchGetsNewDrain(t *testing.T) {
	f := latchedVision(t, true)
	ctx := context.Background()
	if err := f.c.EnforceLimits(ctx); !errors.Is(err, ErrLimitStopDeferred) {
		t.Fatalf("deferral = %v", err)
	}
	state := f.state(t)
	state.Health, state.Phase, state.Admission, state.ActiveWorkload = control.HealthHealthy, control.PhaseStable, control.AdmissionOpen, "vision"
	if _, err := f.s.UpdateState(ctx, state.Version, state); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.record(t); ok {
		t.Fatal("the record outlives the latch")
	}
	f.offset = f.c.config.DrainTimeout + time.Millisecond
	f.r.healthErr = limitDrift
	if _, err := f.c.Status(ctx); !errors.Is(err, gpuruntime.ErrLimitDrift) {
		t.Fatalf("second latch = %v", err)
	}
	f.r.healthErr = nil
	if err := f.c.EnforceLimits(ctx); !errors.Is(err, ErrLimitStopDeferred) || f.stopCount() != 0 {
		t.Fatalf("tick of the new latch = %v; calls %v", err, f.r.calls)
	}
}

// Work cut off by a stop is reported also when the release check after
// the stop fails.
func TestLimitStopReportsCutOffAfterFailedRelease(t *testing.T) {
	f := latchedVision(t, true)
	f.c.config.VerifyTimeout = 100 * time.Millisecond
	f.r.releaseErr = errors.New("compute process remains")
	if err := f.c.EnforceLimits(context.Background()); !errors.Is(err, ErrLimitStopDeferred) {
		t.Fatalf("deferral = %v", err)
	}
	f.offset = f.c.config.DrainTimeout + time.Millisecond
	err := f.c.EnforceLimits(context.Background())
	if !errors.Is(err, ErrLimitStopped) || !errors.Is(err, f.r.releaseErr) || !strings.Contains(err.Error(), "cut off") {
		t.Fatalf("stop with a failed release = %v", err)
	}
}

// twoLimitedUnits latches a state while vision runs, with a second unit
// with limits, audio, in the catalog.
func twoLimitedUnits(t *testing.T) *limitFixture {
	t.Helper()
	catalog := limitedCatalog()
	audio := catalog.Profiles[2]
	audio.ID, audio.Label, audio.Unit = "audio", "Audio", "gws-owned-audio.service"
	audio.Cgroup = "/user.slice/user-1000.slice/user@1000.service/app.slice/gws-owned-audio.service"
	audio.HealthURL = "http://127.0.0.1:9103/health"
	model := *audio.NativeModel
	owned := *model.Owned
	model.Endpoint, model.LaunchFile = "http://127.0.0.1:9103", "/home/u/.config/systemd/user/gws-owned-audio.service"
	owned.Port, owned.Alias, model.Model, model.Instance = 9103, "audio-q8", "audio-q8", "audio"
	model.Owned = &owned
	audio.NativeModel = &model
	catalog.Profiles = append(catalog.Profiles, audio)
	c, lr, s := limitedController(t, catalog)
	f := &limitFixture{c: c, r: &stoppingRuntime{limitedRuntime: lr}, s: s}
	c.runtime = f.r
	base := time.Now()
	c.now = func() time.Time { return base.Add(f.offset) }
	ctx := context.Background()
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Switch(ctx, "vision", "test"); err != nil {
		t.Fatal(err)
	}
	lr.healthErr = limitDrift
	if _, err := c.Status(ctx); err == nil {
		t.Fatal("no latch")
	}
	lr.healthErr = nil
	lr.calls = nil
	return f
}

// A definite failure of a later unit comes before an unproven failure of
// an earlier unit, so the later unit stops at once.
func TestLimitTickPrefersDefiniteFailure(t *testing.T) {
	f := twoLimitedUnits(t)
	c, lr, ctx := f.c, f.r.limitedRuntime, context.Background()
	lr.alsoActive = []control.Workload{"audio"}
	lr.protectionErrs = map[control.Workload]error{"vision": gpuruntime.ErrLimitReadback, "audio": limitDrift}
	lr.calls = nil
	err := c.EnforceLimits(ctx)
	if !errors.Is(err, ErrLimitStopped) || !slices.Contains(lr.calls, "stop audio") || slices.Contains(lr.calls, "stop vision") {
		t.Fatalf("enforce = %v; calls %v", err, lr.calls)
	}
	if got := f.stops(t); len(got) != 1 || got[0][1] != "observation/limit stop reason audio/definite check failure; limit outcome: kill (memory.events oom_kill 1)" {
		t.Fatalf("journal = %q", got)
	}
}

// A crash between the start and the end of a stop leaves its transition in
// progress. Each later tick exits nonzero with "run recover" at once, and
// none stops the unit a second time.
func TestLimitTickInterruptedStopEscalates(t *testing.T) {
	f := latchedVision(t, false)
	ctx := context.Background()
	f.c.store = &limitStore{storeGateway: f.s, finishErr: errors.New("store failed")}
	if err := f.c.EnforceLimits(ctx); !errors.Is(err, ErrLimitStopped) || f.stopCount() != 1 {
		t.Fatalf("interrupted stop = %v", err)
	}
	f.c.store = f.s
	var log bytes.Buffer
	for range 2 {
		err := f.c.LimitTick(ctx, &log)
		if !errors.Is(err, ErrLimitStopFailed) || !errors.Is(err, ErrTransitionRunning) || !strings.Contains(err.Error(), "run recover") || f.stopCount() != 1 {
			t.Fatalf("tick after the crash = %v; calls %v", err, f.r.calls)
		}
	}
}

// bumpingStore changes the state between the read of EnforceLimits and the
// start of the stop transition.
type bumpingStore struct {
	*store.Store
}

func (s bumpingStore) StartTransition(ctx context.Context, version uint64, tr store.Transition) (control.State, error) {
	state, err := s.Store.State(ctx)
	if err != nil {
		return control.State{}, err
	}
	if _, err := s.Store.UpdateState(ctx, state.Version, state); err != nil {
		return control.State{}, err
	}
	return s.Store.StartTransition(ctx, version, tr)
}

// A state change after the read refuses the stop: no stop and no
// transition.
func TestLimitStopRefusedOnStateChange(t *testing.T) {
	f := latchedVision(t, false)
	f.c.store = bumpingStore{f.s}
	err := f.c.EnforceLimits(context.Background())
	if !errors.Is(err, ErrLimitStopFailed) || !errors.Is(err, store.ErrVersionConflict) || f.stopCount() != 0 || len(f.stops(t)) != 0 {
		t.Fatalf("stop after a state change = %v; calls %v", err, f.r.calls)
	}
}

// After a failed stop, the next tick stops the unit again, also when its
// check now passes or proves nothing, or when the pending work read fails.
// No check, record or deferral replaces the started stop.
func TestLimitTickRetriesStartedStop(t *testing.T) {
	ctx := context.Background()
	for name, edit := range map[string]func(*limitFixture){
		"passing check":  func(f *limitFixture) { f.r.protectionErr = nil },
		"unproven check": func(f *limitFixture) { f.r.protectionErr = gpuruntime.ErrLimitReadback },
		"pending work unread": func(f *limitFixture) {
			f.c.store = &limitStore{storeGateway: f.s, pendingErr: errors.New("store failed")}
		},
	} {
		f := latchedVision(t, true)
		if err := f.c.EnforceLimits(ctx); !errors.Is(err, ErrLimitStopDeferred) {
			t.Fatalf("%s: deferral = %v", name, err)
		}
		f.offset = f.c.config.DrainTimeout + time.Millisecond
		f.r.visionStopErr = errors.New("stop refused")
		if err := f.c.EnforceLimits(ctx); !errors.Is(err, ErrLimitStopFailed) {
			t.Fatalf("%s: failed stop = %v", name, err)
		}
		f.r.visionStopErr = nil
		edit(f)
		err := f.c.EnforceLimits(ctx)
		if !errors.Is(err, ErrLimitStopped) || !strings.Contains(err.Error(), "retried the earlier limit stop") || f.stopCount() != 2 ||
			!strings.Contains(err.Error(), "resolve-work") {
			t.Errorf("%s: retry = %v; calls %v", name, err, f.r.calls)
		}
		if stop, ok := f.record(t); !ok || stop.StoppingAt.IsZero() {
			t.Errorf("%s: record = %+v, %v", name, stop, ok)
		}
	}
}

// After a started stop, a failed observation or catalog check exits
// nonzero at once: a committed stop keeps ErrLimitStopped.
func TestLimitTickStartedStopStepFailures(t *testing.T) {
	ctx := context.Background()
	f := latchedVision(t, false)
	if err := f.c.EnforceLimits(ctx); !errors.Is(err, ErrLimitStopped) {
		t.Fatalf("stop = %v", err)
	}
	f.r.observeErr = errors.New("bus down")
	err := f.c.EnforceLimits(ctx)
	if !errors.Is(err, ErrLimitStopped) || !errors.Is(err, f.r.observeErr) || errors.Is(err, ErrLimitStopOverdue) {
		t.Fatalf("observation failure after the stop = %v", err)
	}
	f.c.store = &limitStore{storeGateway: f.s, stateErr: errors.New("state unreadable")}
	if err := f.c.EnforceLimits(ctx); !errors.Is(err, ErrLimitStopFailed) || !strings.Contains(err.Error(), "run recover") {
		t.Fatalf("state failure after the stop = %v", err)
	}
}

// Two unproven failures record the first unit of the catalog.
func TestLimitTickRecordsFirstUnprovenFailure(t *testing.T) {
	f := twoLimitedUnits(t)
	f.r.alsoActive = []control.Workload{"audio"}
	f.r.protectionErrs = map[control.Workload]error{"vision": gpuruntime.ErrLimitReadback, "audio": gpuruntime.ErrLimitReadback}
	if err := f.c.EnforceLimits(context.Background()); !errors.Is(err, ErrLimitStopDeferred) {
		t.Fatalf("enforce = %v", err)
	}
	if stop, ok := f.record(t); !ok || stop.Workload != "vision" {
		t.Fatalf("record = %+v, %v", stop, ok)
	}
}

// A stop whose release check failed keeps its record. The started stop
// takes precedence over an unproven failure of another unit: the tick
// only probes the release again and exits nonzero.
func TestLimitTickStartedStopKeepsRecordAgainstOtherUnit(t *testing.T) {
	f := twoLimitedUnits(t)
	ctx := context.Background()
	f.c.config.VerifyTimeout = 100 * time.Millisecond
	f.r.protectionErr = limitDrift
	f.r.releaseErr = errors.New("compute process remains")
	if err := f.c.EnforceLimits(ctx); !errors.Is(err, ErrLimitStopped) || !errors.Is(err, f.r.releaseErr) {
		t.Fatalf("stop = %v", err)
	}
	f.r.protectionErr = nil
	f.r.alsoActive = []control.Workload{"audio"}
	f.r.protectionErrs = map[control.Workload]error{"audio": gpuruntime.ErrLimitReadback}
	var log bytes.Buffer
	err := f.c.LimitTick(ctx, &log)
	if !errors.Is(err, ErrLimitStopFailed) || errors.Is(err, ErrLimitStopDeferred) || f.stopCount() != 1 ||
		slices.Contains(f.r.calls, "stop audio") || !strings.Contains(err.Error(), "release check") {
		t.Fatalf("tick with another unit = %v; calls %v", err, f.r.calls)
	}
	if stop, ok := f.record(t); !ok || stop.Workload != "vision" || stop.StoppingAt.IsZero() {
		t.Fatalf("record = %+v, %v", stop, ok)
	}
}

// A failed write of the release check observation fails the transition,
// and the workload is unknown.
func TestLimitStopReleaseJournalFailure(t *testing.T) {
	f := latchedVision(t, false)
	boom := errors.New("journal failed")
	f.c.store = &limitStore{storeGateway: f.s, eventErr: boom, eventAction: "limit stop release check"}
	err := f.c.EnforceLimits(context.Background())
	if !errors.Is(err, ErrLimitStopped) || !errors.Is(err, boom) {
		t.Fatalf("stop = %v", err)
	}
	if state := f.state(t); state.ActiveWorkload != control.WorkloadUnknown {
		t.Fatalf("state = %+v", state)
	}
	if got := f.stops(t); len(got) != 1 || got[0][0] != "failed" {
		t.Fatalf("journal = %q", got)
	}
}

// When the probe of an earlier stop passes but the stop again fails, the
// tick gives ErrLimitStopFailed, and a second transition fails.
func TestLimitTickFinishEarlierStopFails(t *testing.T) {
	f := latchedVision(t, false)
	ctx := context.Background()
	f.c.config.VerifyTimeout = 100 * time.Millisecond
	f.r.releaseErr = errors.New("compute process remains")
	if err := f.c.EnforceLimits(ctx); !errors.Is(err, ErrLimitStopped) {
		t.Fatalf("stop = %v", err)
	}
	f.r.releaseErr = nil
	f.r.visionStopErr = errors.New("stop refused")
	err := f.c.EnforceLimits(ctx)
	if !errors.Is(err, ErrLimitStopFailed) || errors.Is(err, ErrLimitStopped) || !errors.Is(err, f.r.visionStopErr) {
		t.Fatalf("finish = %v", err)
	}
	if state := f.state(t); state.ActiveWorkload != control.WorkloadUnknown {
		t.Fatalf("state = %+v", state)
	}
	if got := f.stops(t); len(got) != 2 || got[1][0] != "failed" {
		t.Fatalf("journal = %q", got)
	}
}

// cancellingRuntime cancels the tick during the stop.
type cancellingRuntime struct {
	*stoppingRuntime
	cancel func()
}

func (r *cancellingRuntime) Stop(ctx context.Context, w control.Workload) error {
	r.cancel()
	return r.stoppingRuntime.Stop(ctx, w)
}

// A tick cancelled during the stop still ends the transition.
func TestLimitStopEndsTransitionAfterCancel(t *testing.T) {
	f := latchedVision(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.c.runtime = &cancellingRuntime{stoppingRuntime: f.r, cancel: cancel}
	if err := f.c.EnforceLimits(ctx); !errors.Is(err, ErrLimitStopped) {
		t.Fatalf("cancelled stop = %v", err)
	}
	if running, err := f.s.InProgressTransition(context.Background()); err != nil || running != "" {
		t.Fatalf("transition in progress = %q, %v", running, err)
	}
	if last := f.r.stopCtxErrs[len(f.r.stopCtxErrs)-1]; last != nil {
		t.Fatalf("stop ran on a spent context: %v", last)
	}
	requireLatchedIdle(t, f)
}

// Only a supervisor-owned state with error health and closed admission
// gets the limit stop.
func TestLimitLatched(t *testing.T) {
	latched := control.State{Owner: control.OwnerSupervisor, Health: control.HealthError, Admission: control.AdmissionClosed}
	open := latched
	open.Admission = control.AdmissionOpen
	user := latched
	user.Owner = control.OwnerUser
	if !limitLatched(latched) || limitLatched(open) || limitLatched(user) {
		t.Fatal("limitLatched")
	}
}

// When the probe of an earlier stop passes, the stop runs again, and its
// release poll then fails, the tick keeps ErrLimitStopped.
func TestLimitTickFinishEarlierStopReleaseFails(t *testing.T) {
	f := latchedVision(t, false)
	ctx := context.Background()
	f.c.config.VerifyTimeout = 100 * time.Millisecond
	failed := errors.New("compute process remains")
	f.r.releaseErr = failed
	if err := f.c.EnforceLimits(ctx); !errors.Is(err, ErrLimitStopped) {
		t.Fatalf("stop = %v", err)
	}
	f.r.releaseErrs = []error{nil}
	err := f.c.EnforceLimits(ctx)
	if !errors.Is(err, ErrLimitStopped) || errors.Is(err, ErrLimitStopFailed) || !errors.Is(err, failed) || f.stopCount() != 2 {
		t.Fatalf("finish = %v; calls %v", err, f.r.calls)
	}
	if got := f.stops(t); len(got) != 2 || got[1][0] != "failed" {
		t.Fatalf("journal = %q", got)
	}
}

// A started stop keeps its record when the catalog no longer has a unit
// with limits, and the tick exits nonzero.
func TestLimitTickStartedStopSurvivesCatalogChange(t *testing.T) {
	f := latchedVision(t, false)
	ctx := context.Background()
	if _, err := f.s.MarkLimitFailure(ctx, "vision", true, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := f.s.MarkLimitStopping(ctx, "vision", time.Now()); err != nil {
		t.Fatal(err)
	}
	f.c.config.Catalog.Catalog = testCatalog()
	if err := f.c.EnforceLimits(ctx); !errors.Is(err, ErrLimitStopFailed) || !strings.Contains(err.Error(), "run recover") {
		t.Fatalf("enforce = %v", err)
	}
	if _, ok := f.record(t); !ok {
		t.Fatal("the started stop lost its record")
	}
}

// A retried stop that fails again keeps ErrLimitStopFailed, and both
// transitions fail with an unknown workload.
func TestLimitTickRetriedStopFailsAgain(t *testing.T) {
	f := latchedVision(t, false)
	ctx := context.Background()
	f.r.visionStopErr = errors.New("stop refused")
	for range 2 {
		err := f.c.EnforceLimits(ctx)
		if !errors.Is(err, ErrLimitStopFailed) || errors.Is(err, ErrLimitStopped) {
			t.Fatalf("stop = %v", err)
		}
	}
	if f.stopCount() != 2 || f.state(t).ActiveWorkload != control.WorkloadUnknown {
		t.Fatalf("calls %v; state %+v", f.r.calls, f.state(t))
	}
	if got := f.stops(t); len(got) != 2 || got[0][0] != "failed" || got[1][0] != "failed" {
		t.Fatalf("journal = %q", got)
	}
}

// After a refused stop, the record is started but no stop ran. While the
// unit runs, the next tick stops it. Once it exits by itself, a failed
// release probe exits nonzero, and an Idle state still needs the probe.
func TestLimitTickAfterRefusedStop(t *testing.T) {
	ctx := context.Background()
	f := latchedVision(t, false)
	f.c.store = bumpingStore{f.s}
	if err := f.c.EnforceLimits(ctx); !errors.Is(err, ErrLimitStopFailed) {
		t.Fatalf("refused stop = %v", err)
	}
	f.c.store = f.s
	if err := f.c.EnforceLimits(ctx); !errors.Is(err, ErrLimitStopped) || f.stopCount() != 1 {
		t.Fatalf("tick after the refusal = %v; calls %v", err, f.r.calls)
	}
	requireLatchedIdle(t, f)

	g := latchedVision(t, false)
	g.c.store = bumpingStore{g.s}
	if err := g.c.EnforceLimits(ctx); !errors.Is(err, ErrLimitStopFailed) {
		t.Fatalf("refused stop = %v", err)
	}
	g.c.store = g.s
	g.r.visionActive = false
	g.r.releaseErr = errors.New("compute process remains")
	err := g.c.EnforceLimits(ctx)
	if !errors.Is(err, ErrLimitStopFailed) || !strings.Contains(err.Error(), "is not finished") || g.stopCount() != 0 {
		t.Fatalf("tick with an exited unit = %v; calls %v", err, g.r.calls)
	}
	state := g.state(t)
	state.ActiveWorkload = control.WorkloadIdle
	if _, err := g.s.UpdateState(ctx, state.Version, state); err != nil {
		t.Fatal(err)
	}
	if err := g.c.EnforceLimits(ctx); !errors.Is(err, ErrLimitStopFailed) || errors.Is(err, ErrLimitStopped) {
		t.Fatalf("idle state with a failed probe = %v", err)
	}
	g.r.releaseErr = nil
	if err := g.c.EnforceLimits(ctx); !errors.Is(err, ErrLimitStopped) || !strings.Contains(err.Error(), "holds the error state") {
		t.Fatalf("idle state with a passed probe = %v", err)
	}
}

// A failed pending work read of a recorded failure escalates once the
// failure is overdue, and stops nothing.
func TestLimitTickPendingReadEscalates(t *testing.T) {
	f := latchedVision(t, false)
	ctx := context.Background()
	f.r.protectionErr = gpuruntime.ErrLimitReadback
	if err := f.c.EnforceLimits(ctx); !errors.Is(err, ErrLimitStopDeferred) {
		t.Fatalf("unproven failure = %v", err)
	}
	f.r.protectionErr = limitDrift
	f.offset = f.c.config.DrainTimeout + f.c.config.VerifyTimeout + time.Millisecond
	f.c.store = &limitStore{storeGateway: f.s, pendingErr: errors.New("store failed")}
	err := f.c.EnforceLimits(ctx)
	if !errors.Is(err, ErrLimitStopOverdue) || errors.Is(err, ErrLimitStopDeferred) || f.stopCount() != 0 {
		t.Fatalf("overdue pending read = %v; calls %v", err, f.r.calls)
	}
}

// A stop that does not run names the pending admitted work and tells the
// operator to run resolve-work. A later tick that finds the unit stopped
// names the cut-off work too.
func TestLimitStopNamesWorkWhenStopFails(t *testing.T) {
	f := latchedVision(t, true)
	ctx := context.Background()
	f.c.config.VerifyTimeout = 100 * time.Millisecond
	if err := f.c.EnforceLimits(ctx); !errors.Is(err, ErrLimitStopDeferred) {
		t.Fatalf("deferral = %v", err)
	}
	f.offset = f.c.config.DrainTimeout + time.Millisecond
	f.r.visionStopErr = errors.New("stop refused")
	err := f.c.EnforceLimits(ctx)
	if !errors.Is(err, ErrLimitStopFailed) || !strings.Contains(err.Error(), "the next stop cuts it off; run resolve-work") {
		t.Fatalf("failed stop = %v", err)
	}
	if err := f.c.EnforceLimits(ctx); !errors.Is(err, ErrLimitStopFailed) || !strings.Contains(err.Error(), "the next stop cuts it off") {
		t.Fatalf("failed retry = %v", err)
	}
	f.r.visionStopErr = nil
	f.r.visionActive = false
	f.r.releaseErr = errors.New("compute process remains")
	err = f.c.EnforceLimits(ctx)
	if !errors.Is(err, ErrLimitStopFailed) || !strings.Contains(err.Error(), "was cut off; run resolve-work") {
		t.Fatalf("finish with a failed probe = %v", err)
	}
}

// slowStopRuntime stops only when the action context ends, so the stop
// uses its whole action timeout.
type slowStopRuntime struct {
	*stoppingRuntime
}

func (r *slowStopRuntime) Stop(ctx context.Context, w control.Workload) error {
	<-ctx.Done()
	return r.stoppingRuntime.Stop(context.WithoutCancel(ctx), w)
}

// A stop that uses its whole action timeout still gets its observation in
// the journal, because the stop context also covers FinalizeTimeout.
func TestLimitStopJournalsSlowStop(t *testing.T) {
	f := latchedVision(t, false)
	f.c.config.ActionTimeout, f.c.config.CleanupTimeout = 50*time.Millisecond, 50*time.Millisecond
	f.c.runtime = &slowStopRuntime{f.r}
	err := f.c.EnforceLimits(context.Background())
	got := f.stops(t)
	if len(got) != 1 || !slices.Contains(got[0], "observation/limit stop vision/ok") {
		t.Fatalf("slow stop = %v; journal %q", err, got)
	}
}

// longStopRuntime takes a stop of 150ms, or ends early when its context
// ends.
type longStopRuntime struct {
	*stoppingRuntime
}

func (r *longStopRuntime) Stop(ctx context.Context, w control.Workload) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(150 * time.Millisecond):
	}
	return r.stoppingRuntime.Stop(ctx, w)
}

// With an action timeout above the cleanup and finalize timeouts, the stop
// gets its whole action budget, and its observation is journaled.
func TestLimitStopGetsWholeActionTimeout(t *testing.T) {
	f := latchedVision(t, false)
	f.c.config.ActionTimeout = 300 * time.Millisecond
	f.c.config.CleanupTimeout, f.c.config.FinalizeTimeout = 20*time.Millisecond, 20*time.Millisecond
	f.c.runtime = &longStopRuntime{f.r}
	err := f.c.EnforceLimits(context.Background())
	got := f.stops(t)
	if !errors.Is(err, ErrLimitStopped) || f.stopCount() != 1 || len(got) != 1 ||
		!slices.Contains(got[0], "intent/limit stop vision/") || !slices.Contains(got[0], "observation/limit stop vision/ok") {
		t.Fatalf("long stop = %v; journal %q", err, got)
	}
}

// failedPeerRuntime has a failed peer unit: the strict observation fails,
// and the recovery observation reports the peer as not running.
type failedPeerRuntime struct {
	*stoppingRuntime
}

var errFailedPeer = errors.New("media.service is neither running nor stopped")

func (r *failedPeerRuntime) Observe(context.Context) (gpuruntime.Snapshot, error) {
	return gpuruntime.Snapshot{}, errFailedPeer
}

func (r *failedPeerRuntime) ObserveForRecovery(ctx context.Context) (gpuruntime.Snapshot, error) {
	return r.stoppingRuntime.Observe(ctx)
}

// A failed peer unit does not hide a running unit with limits: a definite
// failure still stops it, and a failure that proves nothing exits nonzero
// with the peer failure instead of a skip.
func TestLimitTickChecksUnitBesideFailedPeer(t *testing.T) {
	ctx := context.Background()
	f := latchedVision(t, false)
	f.c.runtime = &failedPeerRuntime{f.r}
	err := f.c.EnforceLimits(ctx)
	if !errors.Is(err, ErrLimitStopped) || !errors.Is(err, errFailedPeer) || f.stopCount() != 1 {
		t.Fatalf("definite failure beside a failed peer = %v; calls %v", err, f.r.calls)
	}

	g := latchedVision(t, false)
	g.c.runtime = &failedPeerRuntime{g.r}
	g.r.protectionErr = gpuruntime.ErrLimitReadback
	var log bytes.Buffer
	err = g.c.LimitTick(ctx, &log)
	if err == nil || !errors.Is(err, errFailedPeer) || !strings.Contains(err.Error(), "deferred") || g.stopCount() != 0 {
		t.Fatalf("unproven failure beside a failed peer = %v; calls %v", err, g.r.calls)
	}
	if stop, ok := g.record(t); !ok || stop.Workload != "vision" {
		t.Fatalf("record = %+v, %v", stop, ok)
	}

	h := latchedVision(t, false)
	h.c.runtime = &failedPeerRuntime{h.r}
	h.r.protectionErr = nil
	if err := h.c.LimitTick(ctx, &log); !errors.Is(err, errFailedPeer) {
		t.Fatalf("passing check beside a failed peer = %v", err)
	}
}
