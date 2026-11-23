package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

var policyEpoch = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// policyFixture opens a store with a controllable clock and moves it into an
// open, stable, supervisor-owned text workload with an accepted catalog.
func policyFixture(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	now := policyEpoch
	var index atomic.Int64
	uuid := func() (string, error) {
		return fmt.Sprintf("00000000-0000-4000-8000-%012d", index.Add(1)), nil
	}
	s, err := open(context.Background(), filepath.Join(t.TempDir(), "state.db"), func() time.Time { return now }, uuid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	_, err = s.ReplaceCatalog(ctx, "", control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{
		{ID: "text", Label: "Text", Adapter: "systemd", Unit: "text.service", Cgroup: "/user/text", HealthURL: "http://localhost:9000"},
		{ID: "media", Label: "Media", Adapter: "systemd", Unit: "media.service", Cgroup: "/user/media", HealthURL: "http://localhost:9001"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state.Phase = control.PhaseStable
	state.DesiredWorkload = control.WorkloadText
	state.ActiveWorkload = control.WorkloadText
	state.Admission = control.AdmissionOpen
	if _, err := s.UpdateState(ctx, state.Version, state); err != nil {
		t.Fatal(err)
	}
	return s, &now
}

// enablePolicy commits a 5-minute idle policy and returns the fresh settings
// revision.
func enablePolicy(t *testing.T, s *Store) string {
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
	committed, err := s.SetIdlePolicy(ctx, e, control.IdlePolicy{TimeoutMinutes: 5})
	if err != nil {
		t.Fatal(err)
	}
	return committed.SettingsRevision
}

// idleDrain builds a valid drain-to-idle transition for the current state.
func idleDrain(t *testing.T, s *Store, id string) Transition {
	t.Helper()
	ctx := context.Background()
	current, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := s.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	target := current
	target.DesiredWorkload = control.WorkloadIdle
	target.ActiveWorkload = control.WorkloadIdle
	target.Admission = control.AdmissionClosed
	return Transition{
		ID: id, Source: current, Target: target, Previous: current,
		ConfigurationRevision: snap.Revision, Deadline: time.Now().Add(time.Minute),
	}
}

// armForTest writes an armed deadline row as a database from an older
// release could hold it. No production path arms a deadline any more, but
// the write paths still clear such a row.
func armForTest(t *testing.T, s *Store, deadline, attestedAt time.Time) {
	t.Helper()
	if err := armRow(context.Background(), s, deadline, attestedAt); err != nil {
		t.Fatal(err)
	}
}

func armRow(ctx context.Context, s *Store, deadline, attestedAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE idle_policy_state SET armed_deadline = ?, attestation_at = ? WHERE singleton = 1`,
		formatTime(deadline), formatTime(attestedAt))
	return err
}

// armedFixture enables the policy and arms the deadline computed from the
// seeded activity time; the returned clock is still before it.
func armedFixture(t *testing.T) (*Store, *time.Time, time.Time) {
	t.Helper()
	s, now := policyFixture(t)
	enablePolicy(t, s)
	deadline := policyEpoch.Add(5 * time.Minute)
	armForTest(t, s, deadline, policyEpoch.Add(4*time.Minute))
	return s, now, deadline
}

// idleFixture enables the 5-minute policy on the seeded activity time and
// returns the idle deadline; the returned clock is still before it.
func idleFixture(t *testing.T) (*Store, *time.Time, time.Time) {
	t.Helper()
	s, now := policyFixture(t)
	enablePolicy(t, s)
	return s, now, policyEpoch.Add(5 * time.Minute)
}

// resetActivity moves last activity back to the seeded time, so a guard
// other than the deadline is the one under test.
func resetActivity(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(), `UPDATE idle_policy_state SET last_activity_at = ? WHERE singleton = 1`, formatTime(policyEpoch)); err != nil {
		t.Fatal(err)
	}
}

func TestStartIdleTransitionFiresAfterTimeout(t *testing.T) {
	s, now, deadline := idleFixture(t)
	ctx := context.Background()
	tr := idleDrain(t, s, "idle-1")
	*now = deadline // exactly at the deadline: elapsed
	started, err := s.StartIdleTransition(ctx, tr)
	if err != nil {
		t.Fatal(err)
	}
	if started.Phase != control.PhaseDraining || started.Admission != control.AdmissionClosed || started.DesiredWorkload != control.WorkloadIdle {
		t.Fatalf("idle transition start = %+v", started)
	}
	if running, err := s.InProgressTransition(ctx); err != nil || running != tr.ID {
		t.Fatalf("running transition = %q %v", running, err)
	}
}

func TestStartIdleTransitionPreemptsOnAnyMismatch(t *testing.T) {
	for _, mode := range []string{"not elapsed", "activity before deadline", "pending work", "queued demand", "reserved demand",
		"policy off", "active idle", "degraded health", "user owner", "stale version", "running transition"} {
		t.Run(mode, func(t *testing.T) {
			s, now, deadline := idleFixture(t)
			ctx := context.Background()
			*now = deadline // elapsed by default
			tr := idleDrain(t, s, "idle-1")
			switch mode {
			case "not elapsed":
				*now = deadline.Add(-time.Second)
			case "activity before deadline":
				*now = deadline.Add(-time.Minute)
				state, _ := s.State(ctx)
				if err := s.AdmitWork(ctx, "req", "job", control.WorkloadText, state.LeaseFence); err != nil {
					t.Fatal(err)
				}
				if err := s.FinishWorkFenced(ctx, "req", control.WorkloadText, state.LeaseFence, WorkCompleted); err != nil {
					t.Fatal(err)
				}
				*now = deadline
			case "pending work":
				state, _ := s.State(ctx)
				if err := s.AdmitWork(ctx, "req", "job", control.WorkloadText, state.LeaseFence); err != nil {
					t.Fatal(err)
				}
				resetActivity(t, s)
			case "queued demand":
				if err := s.QueueDemand(ctx, testDemand()); err != nil {
					t.Fatal(err)
				}
				resetActivity(t, s)
			case "reserved demand":
				if err := s.QueueDemand(ctx, testDemand()); err != nil {
					t.Fatal(err)
				}
				if _, err := s.ReserveDemand(ctx, testDemand()); err != nil {
					t.Fatal(err)
				}
				resetActivity(t, s)
			case "policy off":
				if _, err := s.db.ExecContext(ctx, `UPDATE operator_settings SET idle_timeout_minutes = 0 WHERE singleton = 1`); err != nil {
					t.Fatal(err)
				}
			case "active idle":
				// Read-model corruption a real path cannot produce; the idle
				// guard must still preempt rather than drain idle into idle.
				if _, err := s.db.ExecContext(ctx, `UPDATE control_state SET active_workload = 'idle' WHERE singleton = 1`); err != nil {
					t.Fatal(err)
				}
			case "degraded health":
				state, _ := s.State(ctx)
				state.Health = control.HealthDegraded
				if _, err := s.UpdateState(ctx, state.Version, state); err != nil {
					t.Fatal(err)
				}
				tr = idleDrain(t, s, "idle-1")
			case "user owner":
				state, _ := s.State(ctx)
				state.Owner = control.OwnerUser
				state.Admission = control.AdmissionClosed
				if _, err := s.UpdateState(ctx, state.Version, state); err != nil {
					t.Fatal(err)
				}
				tr = idleDrain(t, s, "idle-1")
			case "stale version":
				state, _ := s.State(ctx)
				if _, err := s.UpdateState(ctx, state.Version, state); err != nil {
					t.Fatal(err)
				}
			case "running transition":
				current, _ := s.State(ctx)
				media := current
				media.DesiredWorkload = control.WorkloadMedia
				other := idleDrain(t, s, "other")
				other.Target = media
				if _, err := s.StartTransition(ctx, current.Version, other); err != nil {
					t.Fatal(err)
				}
				tr = idleDrain(t, s, "idle-1")
			}
			before, err := s.State(ctx)
			if err != nil {
				t.Fatalf("%s: %v", mode, err)
			}
			if _, err := s.StartIdleTransition(ctx, tr); !errors.Is(err, ErrPolicyPreempted) {
				t.Fatalf("%s: %v, want ErrPolicyPreempted", mode, err)
			}
			after, err := s.State(ctx)
			if err != nil {
				t.Fatalf("%s: %v", mode, err)
			}
			if after.Version != before.Version || after.Phase != before.Phase {
				t.Fatalf("%s: preempted start changed state: %+v", mode, after)
			}
		})
	}
}

// The tick keeps no state of its own: a restart past the deadline fires
// from the durable activity time and policy alone.
func TestIdleDeadlineFiresAfterReopen(t *testing.T) {
	dir := t.TempDir()
	now := policyEpoch
	var index atomic.Int64
	uuid := func() (string, error) {
		return fmt.Sprintf("00000000-0000-4000-8000-%012d", index.Add(1)), nil
	}
	clock := func() time.Time { return now }
	path := filepath.Join(dir, "state.db")
	ctx := context.Background()
	s, err := open(ctx, path, clock, uuid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplaceCatalog(ctx, "", control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "text", Label: "Text", Adapter: "systemd", Unit: "text.service", Cgroup: "/user/text", HealthURL: "http://localhost:9000"}}}); err != nil {
		t.Fatal(err)
	}
	state, _ := s.State(ctx)
	state.Phase = control.PhaseStable
	state.DesiredWorkload = control.WorkloadText
	state.ActiveWorkload = control.WorkloadText
	state.Admission = control.AdmissionOpen
	if _, err := s.UpdateState(ctx, state.Version, state); err != nil {
		t.Fatal(err)
	}
	enablePolicy(t, s)
	if _, err := s.StartIdleTransition(ctx, idleDrain(t, s, "idle-early")); !errors.Is(err, ErrPolicyPreempted) {
		t.Fatalf("idle before the deadline: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	now = policyEpoch.Add(6 * time.Minute)
	reopened, err := open(ctx, path, clock, uuid)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	started, err := reopened.StartIdleTransition(ctx, idleDrain(t, reopened, "idle-after-restart"))
	if err != nil {
		t.Fatal(err)
	}
	if started.Phase != control.PhaseDraining || started.DesiredWorkload != control.WorkloadIdle {
		t.Fatalf("restart fire = %+v", started)
	}
}

func TestAdmitAndIdleStartAreMutuallyExclusiveInEitherOrder(t *testing.T) {
	t.Run("admit first preempts idle", func(t *testing.T) {
		s, now, deadline := idleFixture(t)
		ctx := context.Background()
		state, _ := s.State(ctx)
		*now = deadline
		tr := idleDrain(t, s, "idle-1")
		if err := s.AdmitWork(ctx, "req", "job", control.WorkloadText, state.LeaseFence); err != nil {
			t.Fatal(err)
		}
		if _, err := s.StartIdleTransition(ctx, tr); !errors.Is(err, ErrPolicyPreempted) {
			t.Fatalf("idle after admit: %v", err)
		}
	})
	t.Run("idle first closes admission", func(t *testing.T) {
		s, now, deadline := idleFixture(t)
		ctx := context.Background()
		*now = deadline
		started, err := s.StartIdleTransition(ctx, idleDrain(t, s, "idle-1"))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.AdmitWork(ctx, "req", "job", control.WorkloadText, started.LeaseFence); !errors.Is(err, ErrAdmissionClosed) {
			t.Fatalf("admit after idle start: %v", err)
		}
	})
}

func TestAdmitAndFinishAdvanceLastActivity(t *testing.T) {
	s, now := policyFixture(t)
	ctx := context.Background()
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := s.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !settings.LastActivityAt.Equal(policyEpoch) {
		t.Fatalf("seeded last activity %v", settings.LastActivityAt)
	}
	*now = policyEpoch.Add(time.Hour)
	if err := s.AdmitWork(ctx, "req-1", "job-1", control.WorkloadText, state.LeaseFence); err != nil {
		t.Fatal(err)
	}
	settings, _ = s.Settings(ctx)
	if !settings.LastActivityAt.Equal(*now) {
		t.Fatalf("admit did not advance activity: %v", settings.LastActivityAt)
	}
	*now = now.Add(time.Hour)
	if err := s.FinishWorkFenced(ctx, "req-1", control.WorkloadText, state.LeaseFence, WorkCompleted); err != nil {
		t.Fatal(err)
	}
	settings, _ = s.Settings(ctx)
	if !settings.LastActivityAt.Equal(*now) {
		t.Fatalf("finish did not advance activity: %v", settings.LastActivityAt)
	}
}

func TestFailedOrReplayedFinishDoesNotRegressActivity(t *testing.T) {
	s, now := policyFixture(t)
	ctx := context.Background()
	state, _ := s.State(ctx)
	*now = policyEpoch.Add(time.Hour)
	if err := s.AdmitWork(ctx, "req-1", "job-1", control.WorkloadText, state.LeaseFence); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishWorkFenced(ctx, "req-1", control.WorkloadText, state.LeaseFence, WorkCompleted); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(-time.Minute) // skewed clock must not move activity backwards
	if err := s.FinishWorkFenced(ctx, "req-1", control.WorkloadText, state.LeaseFence, WorkCompleted); !errors.Is(err, ErrWorkAlreadyCompleted) {
		t.Fatalf("replay: %v", err)
	}
	settings, _ := s.Settings(ctx)
	if !settings.LastActivityAt.Equal(policyEpoch.Add(time.Hour)) {
		t.Fatalf("replay regressed activity: %v", settings.LastActivityAt)
	}
}

func TestCommittedNonIdleTransitionRecordsActivityButIdleCommitDoesNot(t *testing.T) {
	s, now := policyFixture(t)
	ctx := context.Background()
	state, _ := s.State(ctx)
	snap, _ := s.Catalog(ctx)
	media := state
	media.DesiredWorkload = control.WorkloadMedia
	media.ActiveWorkload = control.WorkloadMedia
	media.Admission = control.AdmissionOpen
	tr := Transition{ID: "to-media", Source: state, Target: media, Previous: state, ConfigurationRevision: snap.Revision, Deadline: time.Now().Add(time.Minute)}
	*now = policyEpoch.Add(2 * time.Hour)
	started, err := s.StartTransition(ctx, state.Version, tr)
	if err != nil {
		t.Fatal(err)
	}
	media.LeaseFence = started.LeaseFence
	if _, err := s.FinishTransition(ctx, tr.ID, "committed", started.Version, media); err != nil {
		t.Fatal(err)
	}
	settings, _ := s.Settings(ctx)
	if !settings.LastActivityAt.Equal(*now) {
		t.Fatalf("committed non-idle transition did not record activity: %v", settings.LastActivityAt)
	}
	// Draining back to idle must not count as fresh activity.
	committed, _ := s.State(ctx)
	idle := committed
	idle.DesiredWorkload = control.WorkloadIdle
	idle.ActiveWorkload = control.WorkloadIdle
	idle.Admission = control.AdmissionClosed
	back := Transition{ID: "to-idle", Source: committed, Target: idle, Previous: committed, ConfigurationRevision: snap.Revision, Deadline: time.Now().Add(time.Minute)}
	*now = now.Add(time.Hour)
	started, err = s.StartTransition(ctx, committed.Version, back)
	if err != nil {
		t.Fatal(err)
	}
	idle.LeaseFence = started.LeaseFence
	if _, err := s.FinishTransition(ctx, back.ID, "committed", started.Version, idle); err != nil {
		t.Fatal(err)
	}
	settings, _ = s.Settings(ctx)
	if !settings.LastActivityAt.Equal(policyEpoch.Add(2 * time.Hour)) {
		t.Fatalf("idle commit moved activity: %v", settings.LastActivityAt)
	}
}

// A dangling armed deadline on a non-open/non-stable state is corruption:
// reads fail closed instead of serving an idle that cannot be valid.
func TestReadStateFailsClosedOnDanglingArmedDeadline(t *testing.T) {
	s, _, deadline := armedFixture(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `UPDATE control_state SET admission = 'closed' WHERE singleton = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.State(ctx); err == nil {
		t.Fatal("state with dangling armed deadline validated")
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE control_state SET admission = 'open' WHERE singleton = 1`); err != nil {
		t.Fatal(err)
	}
	state, err := s.State(ctx)
	if err != nil || state.PendingIdleDeadline == nil || !state.PendingIdleDeadline.Equal(deadline) {
		t.Fatalf("restored state: %+v %v", state, err)
	}
}

func TestWritePathsDisarmArmedDeadline(t *testing.T) {
	rearm := func(t *testing.T, s *Store) time.Time {
		t.Helper()
		deadline := policyEpoch.Add(5 * time.Minute)
		armForTest(t, s, deadline, policyEpoch.Add(4*time.Minute))
		return deadline
	}
	assertDisarmed := func(t *testing.T, s *Store) {
		t.Helper()
		settings, err := s.Settings(context.Background())
		if err != nil || settings.ArmedDeadline != nil {
			t.Fatalf("deadline survived write path: %#v %v", settings, err)
		}
	}

	t.Run("operator transition", func(t *testing.T) {
		s, _ := policyFixture(t)
		enablePolicy(t, s)
		rearm(t, s)
		current, _ := s.State(context.Background())
		snap, _ := s.Catalog(context.Background())
		media := current
		media.DesiredWorkload = control.WorkloadMedia
		e := control.OperatorPrecondition{Incarnation: current.LeaseFence.Incarnation, Version: current.Version, Owner: current.Owner, ConfigurationRevision: snap.Revision}
		tr := Transition{ID: "op", Source: current, Target: media, Previous: current, ConfigurationRevision: snap.Revision, Deadline: time.Now().Add(time.Minute)}
		if _, err := s.StartOperatorTransition(context.Background(), e, tr); err != nil {
			t.Fatal(err)
		}
		assertDisarmed(t, s)
	})
	t.Run("legacy transition", func(t *testing.T) {
		s, _ := policyFixture(t)
		enablePolicy(t, s)
		rearm(t, s)
		tr := idleDrain(t, s, "legacy")
		if _, err := s.StartTransition(context.Background(), tr.Source.Version, tr); err != nil {
			t.Fatal(err)
		}
		assertDisarmed(t, s)
	})
	t.Run("settings write", func(t *testing.T) {
		s, _ := policyFixture(t)
		enablePolicy(t, s)
		rearm(t, s)
		current, _ := s.State(context.Background())
		snap, _ := s.Catalog(context.Background())
		settings, _ := s.Settings(context.Background())
		e := control.SettingsPrecondition{Incarnation: current.LeaseFence.Incarnation, Version: current.Version, Owner: current.Owner, ConfigurationRevision: snap.Revision, SettingsRevision: settings.SettingsRevision}
		if _, err := s.SetIdlePolicy(context.Background(), e, control.IdlePolicy{TimeoutMinutes: 30}); err != nil {
			t.Fatal(err)
		}
		assertDisarmed(t, s)
	})
	t.Run("recover", func(t *testing.T) {
		s, _ := policyFixture(t)
		enablePolicy(t, s)
		rearm(t, s)
		current, _ := s.State(context.Background())
		final := current
		final.Phase = control.PhaseReconciling
		final.Health = control.HealthError
		final.Admission = control.AdmissionClosed
		final.PendingIdleDeadline = nil
		if _, err := s.Recover(context.Background(), current.Version, final, "test recovery"); err != nil {
			t.Fatal(err)
		}
		assertDisarmed(t, s)
	})
	t.Run("fence rotation", func(t *testing.T) {
		s, _ := policyFixture(t)
		enablePolicy(t, s)
		rearm(t, s)
		current, _ := s.State(context.Background())
		if _, err := s.RotateFenceAndCloseAdmission(context.Background(), current.Version); err != nil {
			t.Fatal(err)
		}
		assertDisarmed(t, s)
	})
	t.Run("incarnation rotation", func(t *testing.T) {
		s, _ := policyFixture(t)
		enablePolicy(t, s)
		rearm(t, s)
		if _, err := s.RotateIncarnation(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertDisarmed(t, s)
	})
}

// Activity invalidates the armed deadline atomically: the read model (and
// show-settings) must never report a stale verified pending deadline.
func TestActivityAfterArmClearsArmedDeadline(t *testing.T) {
	assertCleared := func(t *testing.T, s *Store) {
		t.Helper()
		settings, err := s.Settings(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if settings.ArmedDeadline != nil || settings.AttestationAt != nil {
			t.Fatalf("stale pending deadline reported: armed=%v attested=%v", settings.ArmedDeadline, settings.AttestationAt)
		}
	}

	t.Run("admit after arm clears", func(t *testing.T) {
		s, now, deadline := armedFixture(t)
		ctx := context.Background()
		state, _ := s.State(ctx)
		*now = deadline
		if err := s.AdmitWork(ctx, "req", "job", control.WorkloadText, state.LeaseFence); err != nil {
			t.Fatal(err)
		}
		assertCleared(t, s)
	})
	t.Run("finish clears an already-armed deadline", func(t *testing.T) {
		// Arming requires zero pending work, so a finish can only land after
		// an arm when the work was admitted after the arm; admit clears first.
		// The finish path therefore funnels through the same touchActivity
		// clearing, exercised here on a deadline armed then disarmed by admit:
		// finishing the admitted work must leave the armed fields cleared.
		s, now, deadline := armedFixture(t)
		ctx := context.Background()
		state, _ := s.State(ctx)
		*now = deadline.Add(-time.Minute)
		if err := s.AdmitWork(ctx, "req", "job", control.WorkloadText, state.LeaseFence); err != nil {
			t.Fatal(err)
		}
		*now = deadline
		if err := s.FinishWorkFenced(ctx, "req", control.WorkloadText, state.LeaseFence, WorkCompleted); err != nil {
			t.Fatal(err)
		}
		assertCleared(t, s)
	})
	t.Run("committed non-idle transition clears", func(t *testing.T) {
		s, now, deadline := armedFixture(t)
		ctx := context.Background()
		state, _ := s.State(ctx)
		snap, _ := s.Catalog(ctx)
		media := state
		media.DesiredWorkload = control.WorkloadMedia
		media.ActiveWorkload = control.WorkloadMedia
		media.Admission = control.AdmissionOpen
		tr := Transition{ID: "to-media", Source: state, Target: media, Previous: state, ConfigurationRevision: snap.Revision, Deadline: time.Now().Add(time.Minute)}
		*now = deadline
		started, err := s.StartTransition(ctx, state.Version, tr)
		if err != nil {
			t.Fatal(err)
		}
		media.LeaseFence = started.LeaseFence
		if _, err := s.FinishTransition(ctx, tr.ID, "committed", started.Version, media); err != nil {
			t.Fatal(err)
		}
		assertCleared(t, s)
	})
}
