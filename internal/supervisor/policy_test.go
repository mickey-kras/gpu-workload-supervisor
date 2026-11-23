package supervisor

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

// idleRuntime counts the cold release checks. An idle stop must use the
// normal release check, like an operator switch to Idle: a media-unload
// workload keeps its service running after an unload.
type idleRuntime struct {
	*fakeRuntime
	coldChecks int
}

func (r *idleRuntime) ReleasedForSetup(ctx context.Context, target control.Workload) error {
	r.coldChecks++
	return r.ReleasedFor(ctx, target)
}

type idleFixture struct {
	path string
	now  time.Time
	s    *store.Store
	r    *idleRuntime
	c    *Controller
}

func (f *idleFixture) clock() time.Time { return f.now }

// newIdleFixture runs the text workload under the supervisor with the given
// idle timeout. The store and the controller share one clock.
func newIdleFixture(t *testing.T, catalog control.Catalog, active control.Workload, timeout int) *idleFixture {
	t.Helper()
	f := &idleFixture{path: filepath.Join(t.TempDir(), "state.db"), now: time.Now().UTC().Truncate(time.Second)}
	s, err := store.OpenWithClock(context.Background(), f.path, f.clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	f.s = s
	if _, err := s.ReplaceCatalog(context.Background(), "", catalog); err != nil {
		t.Fatal(err)
	}
	ownershipState(t, s, control.OwnerSupervisor, active)
	if timeout != control.IdlePolicyOff {
		enableIdlePolicy(t, s, timeout)
	}
	f.r = &idleRuntime{fakeRuntime: &fakeRuntime{active: active}}
	f.c = tickController(t, s, f.r, f.clock)
	return f
}

// pastDeadline moves the clock one minute past last activity plus timeout.
func (f *idleFixture) pastDeadline(t *testing.T, timeout int) {
	t.Helper()
	settings, err := f.s.Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.now = settings.LastActivityAt.Add(time.Duration(timeout+1) * time.Minute)
}

func (f *idleFixture) idleStops(t *testing.T) []string {
	t.Helper()
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT status FROM transitions WHERE initiator = ?`, inactivityPolicyInitiator)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var statuses []string
	for rows.Next() {
		var status string
		if err := rows.Scan(&status); err != nil {
			t.Fatal(err)
		}
		statuses = append(statuses, status)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return statuses
}

func enableIdlePolicy(t *testing.T, s *store.Store, timeout int) control.PolicyState {
	t.Helper()
	ctx := context.Background()
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := s.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := s.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e := control.SettingsPrecondition{
		Incarnation: state.LeaseFence.Incarnation, Version: state.Version, Owner: state.Owner,
		ConfigurationRevision: snap.Revision, SettingsRevision: settings.SettingsRevision,
	}
	committed, err := s.SetIdlePolicy(ctx, e, control.IdlePolicy{TimeoutMinutes: timeout})
	if err != nil {
		t.Fatal(err)
	}
	return committed
}

func tickController(t *testing.T, s *store.Store, r Runtime, now func() time.Time) *Controller {
	t.Helper()
	snapshot, err := s.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	controller, err := newController(s, r, Config{
		Catalog:      &snapshot,
		DrainTimeout: time.Second, VerifyTimeout: time.Second,
		ActionTimeout:  time.Second,
		CleanupTimeout: time.Second, FinalizeTimeout: time.Second,
		PollInterval: time.Millisecond,
	}, now, func() (string, error) {
		return "11111111-1111-4111-8111-111111111111", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return controller
}

func TestIdleTickStopsAfterTimeout(t *testing.T) {
	for _, tick := range []string{"idle tick", "policy tick"} {
		t.Run(tick, func(t *testing.T) {
			f := newIdleFixture(t, testCatalog(), control.WorkloadText, 5)
			f.pastDeadline(t, 5)
			var err error
			if tick == "idle tick" {
				err = f.c.IdleTick(context.Background())
			} else {
				err = f.c.TickWithLimits(context.Background(), io.Discard)
			}
			if err != nil {
				t.Fatal(err)
			}
			state, err := f.s.State(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if state.ActiveWorkload != control.WorkloadIdle || state.Owner != control.OwnerSupervisor ||
				state.Phase != control.PhaseStable || state.Admission != control.AdmissionClosed {
				t.Fatalf("tick did not stop to idle: %+v", state)
			}
			if !strings.Contains(strings.Join(f.r.calls, ","), "stop "+string(control.WorkloadText)) {
				t.Fatalf("workload never stopped: %v", f.r.calls)
			}
			if f.r.coldChecks != 0 {
				t.Fatal("idle stop used the cold release check, which a media-unload workload cannot pass")
			}
			if stops := f.idleStops(t); len(stops) != 1 || stops[0] != "committed" {
				t.Fatalf("journaled idle stops = %v", stops)
			}
			// Idle is not activity, so the next tick has nothing to do.
			if err := f.c.IdleTick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if stops := f.idleStops(t); len(stops) != 1 {
				t.Fatalf("second tick stopped again: %v", stops)
			}
		})
	}
}

func TestIdleTickDoesNothingWhenIdleDoesNotApply(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		timeout int
		setup   func(t *testing.T, f *idleFixture)
	}{
		{name: "policy off", timeout: control.IdlePolicyOff},
		{name: "before the deadline", timeout: 5, setup: func(t *testing.T, f *idleFixture) {
			f.pastDeadline(t, 5)
			f.now = f.now.Add(-2 * time.Minute)
		}},
		{name: "activity before the deadline", timeout: 5, setup: func(t *testing.T, f *idleFixture) {
			f.pastDeadline(t, 5)
			f.now = f.now.Add(-2 * time.Minute)
			state, _ := f.s.State(ctx)
			token, err := f.s.AdmitWorkToken(ctx, "req", "job", control.WorkloadText, state.LeaseFence)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.s.FinishWorkToken(ctx, "req", control.WorkloadText, state.LeaseFence, token, store.WorkCompleted); err != nil {
				t.Fatal(err)
			}
			f.now = f.now.Add(2 * time.Minute)
		}},
		{name: "in-flight work", timeout: 5, setup: func(t *testing.T, f *idleFixture) {
			state, _ := f.s.State(ctx)
			if _, err := f.s.AdmitWorkToken(ctx, "req", "job", control.WorkloadText, state.LeaseFence); err != nil {
				t.Fatal(err)
			}
			f.pastDeadline(t, 5)
		}},
		{name: "queued demand", timeout: 5, setup: func(t *testing.T, f *idleFixture) {
			if err := f.s.QueueDemand(ctx, idleDemand()); err != nil {
				t.Fatal(err)
			}
			f.pastDeadline(t, 5)
		}},
		{name: "reserved demand", timeout: 5, setup: func(t *testing.T, f *idleFixture) {
			if err := f.s.QueueDemand(ctx, idleDemand()); err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.ReserveDemand(ctx, idleDemand()); err != nil {
				t.Fatal(err)
			}
			f.pastDeadline(t, 5)
		}},
		{name: "running transition", timeout: 5, setup: func(t *testing.T, f *idleFixture) {
			current, _ := f.s.State(ctx)
			snap, _ := f.s.Catalog(ctx)
			target := current
			target.DesiredWorkload = control.WorkloadMedia
			if _, err := f.s.StartTransition(ctx, current.Version, store.Transition{ID: "other", Source: current, Target: target,
				Previous: current, ConfigurationRevision: snap.Revision, Deadline: time.Now().Add(time.Minute)}); err != nil {
				t.Fatal(err)
			}
			f.pastDeadline(t, 5)
		}},
		{name: "latched error", timeout: 5, setup: func(t *testing.T, f *idleFixture) {
			state, _ := f.s.State(ctx)
			state.Health = control.HealthError
			state.Admission = control.AdmissionClosed
			if _, err := f.s.UpdateState(ctx, state.Version, state); err != nil {
				t.Fatal(err)
			}
			f.pastDeadline(t, 5)
		}},
		{name: "degraded health", timeout: 5, setup: func(t *testing.T, f *idleFixture) {
			state, _ := f.s.State(ctx)
			state.Health = control.HealthDegraded
			if _, err := f.s.UpdateState(ctx, state.Version, state); err != nil {
				t.Fatal(err)
			}
			f.pastDeadline(t, 5)
		}},
		{name: "user owner", timeout: 5, setup: func(t *testing.T, f *idleFixture) {
			state, _ := f.s.State(ctx)
			state.Owner = control.OwnerUser
			state.Admission = control.AdmissionClosed
			if _, err := f.s.UpdateState(ctx, state.Version, state); err != nil {
				t.Fatal(err)
			}
			f.pastDeadline(t, 5)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newIdleFixture(t, testCatalog(), control.WorkloadText, tc.timeout)
			if tc.setup != nil {
				tc.setup(t, f)
			} else {
				f.now = f.now.Add(48 * time.Hour)
			}
			before, err := f.s.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.c.IdleTick(ctx); err != nil {
				t.Fatalf("tick: %v", err)
			}
			after, err := f.s.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if after.Version != before.Version || after.ActiveWorkload != before.ActiveWorkload {
				t.Fatalf("tick moved the workload: %+v", after)
			}
			if len(f.r.calls) != 0 || len(f.idleStops(t)) != 0 {
				t.Fatalf("tick drove effects: calls=%v stops=%v", f.r.calls, f.idleStops(t))
			}
		})
	}
}

func idleDemand() store.Demand {
	return store.Demand{RequestID: "demand-1", Requester: "requester", Workflow: "workflow", BodySHA256: strings.Repeat("a", 64), Workload: control.WorkloadText}
}

// A trainer is a finite job. The idle policy never stops it, even past the
// deadline with no admitted work.
func TestIdleTickNeverStopsATrainer(t *testing.T) {
	catalog := testCatalog()
	catalog.Version = 4
	catalog.Profiles = append(catalog.Profiles, control.WorkloadProfile{ID: "trainer", Label: "Trainer", Adapter: "systemd", Unit: "trainer.service",
		Cgroup: "/training/trainer.service", RequiredMiB: 1024, BootPolicy: "stop-to-idle", Trainer: &control.TrainerBinding{Version: 1,
			GPUUUID: "GPU-11111111-1111-1111-1111-111111111111", LaunchFile: "/opt/trainer/unit.service", LaunchSHA256: strings.Repeat("a", 64),
			Executable: "/opt/trainer/train", ExecutableSHA256: strings.Repeat("b", 64), RecipeFile: "/opt/trainer/recipe.json",
			RecipeSHA256: strings.Repeat("c", 64), JobFile: "/opt/trainer/job.json", TerminalFile: "/opt/trainer/terminal.json"}})
	f := newIdleFixture(t, catalog, "trainer", 5)
	f.pastDeadline(t, 5)
	if err := f.c.IdleTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := f.s.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.ActiveWorkload != "trainer" || state.Phase != control.PhaseStable || len(f.r.calls) != 0 {
		t.Fatalf("idle policy stopped a trainer: %+v calls=%v", state, f.r.calls)
	}
}

// The tick keeps no state of its own: a fresh process after a restart
// decides from the durable activity time and policy alone.
func TestIdleTickAfterRestartUsesDurableState(t *testing.T) {
	f := newIdleFixture(t, testCatalog(), control.WorkloadText, 5)
	if err := f.c.IdleTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.pastDeadline(t, 5)
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.OpenWithClock(context.Background(), f.path, f.clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	f.s = reopened
	f.c = tickController(t, reopened, f.r, f.clock)
	if err := f.c.IdleTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := reopened.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.ActiveWorkload != control.WorkloadIdle || state.Phase != control.PhaseStable {
		t.Fatalf("restarted tick did not stop to idle: %+v", state)
	}
}

// A request that arrives while the idle stop runs meets closed admission.
// The stop still finishes.
func TestIdleTickClosesAdmissionDuringTheStop(t *testing.T) {
	ctx := context.Background()
	f := newIdleFixture(t, testCatalog(), control.WorkloadText, 5)
	f.pastDeadline(t, 5)
	var admitErr error
	f.r.cancelOnStop = func() {
		draining, err := f.s.State(ctx)
		if err != nil {
			admitErr = err
			return
		}
		_, admitErr = f.s.AdmitWorkToken(ctx, "late", "job", control.WorkloadText, draining.LeaseFence)
	}
	if err := f.c.IdleTick(ctx); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(admitErr, store.ErrAdmissionClosed) {
		t.Fatalf("request during the idle stop: %v", admitErr)
	}
	state, err := f.s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.ActiveWorkload != control.WorkloadIdle || state.Phase != control.PhaseStable {
		t.Fatalf("idle stop did not finish: %+v", state)
	}
}
