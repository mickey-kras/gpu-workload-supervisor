package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
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
	committed, err := s.SetIdlePolicy(ctx, e, control.IdlePolicy{TimeoutMinutes: 5}, true)
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

// armedFixture enables the policy and arms the verified deadline computed
// from the seeded activity time; the returned clock is still before it.
func armedFixture(t *testing.T) (*Store, *time.Time, time.Time) {
	t.Helper()
	s, now := policyFixture(t)
	fingerprint := enablePolicy(t, s)
	deadline := policyEpoch.Add(5 * time.Minute)
	if err := s.ArmIdleDeadline(context.Background(), fingerprint, deadline, policyEpoch.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	return s, now, deadline
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

func TestArmIdleDeadlineRequiresEnabledPolicyFingerprintAndOpenStableState(t *testing.T) {
	s, _ := policyFixture(t)
	ctx := context.Background()
	deadline := policyEpoch.Add(5 * time.Minute)
	settings, _ := s.Settings(ctx)
	if err := s.ArmIdleDeadline(ctx, "stale", deadline, policyEpoch); !errors.Is(err, ErrSettingsConflict) {
		t.Fatalf("fingerprint mismatch: %v", err)
	}
	// Policy still Off: arming is impossible even with the right revision.
	if err := s.ArmIdleDeadline(ctx, settings.SettingsRevision, deadline, policyEpoch); !errors.Is(err, ErrInvalidIdleTimeout) {
		t.Fatalf("arm while off: %v", err)
	}
	fingerprint := enablePolicy(t, s)
	if err := s.ArmIdleDeadline(ctx, fingerprint, deadline, policyEpoch.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	armed, _ := s.Settings(ctx)
	if armed.ArmedDeadline == nil || !armed.ArmedDeadline.Equal(deadline) || armed.AttestationAt == nil {
		t.Fatalf("armed = %#v", armed)
	}
	state, _ := s.State(ctx)
	if state.PendingIdleDeadline == nil || !state.PendingIdleDeadline.Equal(deadline) {
		t.Fatalf("read model pending deadline = %v", state.PendingIdleDeadline)
	}
}

func TestArmIdleDeadlineRejectsClosedAdmission(t *testing.T) {
	s, _ := policyFixture(t)
	ctx := context.Background()
	fingerprint := enablePolicy(t, s)
	state, _ := s.State(ctx)
	state.Admission = control.AdmissionClosed
	if _, err := s.UpdateState(ctx, state.Version, state); err != nil {
		t.Fatal(err)
	}
	if err := s.ArmIdleDeadline(ctx, fingerprint, policyEpoch.Add(5*time.Minute), policyEpoch); !errors.Is(err, ErrPolicyPreempted) {
		t.Fatalf("arm with closed admission: %v", err)
	}
}

func TestStartIdleTransitionFiresArmedElapsedDeadline(t *testing.T) {
	s, now, deadline := armedFixture(t)
	ctx := context.Background()
	tr := idleDrain(t, s, "idle-1")
	*now = deadline // exactly at the deadline: elapsed
	started, err := s.StartIdleTransition(ctx, deadline, tr, nil)
	if err != nil {
		t.Fatal(err)
	}
	if started.Phase != control.PhaseDraining || started.Admission != control.AdmissionClosed || started.DesiredWorkload != control.WorkloadIdle {
		t.Fatalf("idle transition start = %+v", started)
	}
	settings, _ := s.Settings(ctx)
	if settings.ArmedDeadline != nil {
		t.Fatalf("deadline still armed after start: %v", settings.ArmedDeadline)
	}
}

func TestStartIdleTransitionPreemptsOnAnyMismatch(t *testing.T) {
	for _, mode := range []string{"not armed", "armed mismatch", "not elapsed", "pending work", "activity after arm", "policy off", "active idle", "running transition"} {
		t.Run(mode, func(t *testing.T) {
			s, now, deadline := armedFixture(t)
			ctx := context.Background()
			armed := deadline
			*now = deadline // elapsed by default
			switch mode {
			case "not armed":
				if err := s.DisarmIdleDeadline(ctx); err != nil {
					t.Fatal(err)
				}
			case "armed mismatch":
				armed = deadline.Add(time.Minute)
			case "not elapsed":
				*now = deadline.Add(-time.Minute)
			case "pending work":
				// Admitting records newer activity, which alone preempts;
				// restore the marker so the pending-work guard is the one
				// under test.
				state, _ := s.State(ctx)
				if err := s.AdmitWork(ctx, "req", "job", control.WorkloadText, state.LeaseFence); err != nil {
					t.Fatal(err)
				}
				if _, err := s.db.ExecContext(ctx, `UPDATE idle_policy_state SET last_activity_at = ? WHERE singleton = 1`, formatTime(policyEpoch)); err != nil {
					t.Fatal(err)
				}
			case "activity after arm":
				state, _ := s.State(ctx)
				if err := s.AdmitWork(ctx, "req", "job", control.WorkloadText, state.LeaseFence); err != nil {
					t.Fatal(err)
				}
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
			case "running transition":
				current, _ := s.State(ctx)
				media := current
				media.DesiredWorkload = control.WorkloadMedia
				tr := idleDrain(t, s, "other")
				tr.Target = media
				if _, err := s.StartTransition(ctx, current.Version, tr); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.StartIdleTransition(ctx, armed, idleDrain(t, s, "idle-1"), nil); !errors.Is(err, ErrPolicyPreempted) {
				t.Fatalf("%s: %v, want ErrPolicyPreempted", mode, err)
			}
			after, err := s.State(ctx)
			if err != nil {
				t.Fatalf("%s: %v", mode, err)
			}
			wantPhase := control.PhaseStable
			if mode == "running transition" {
				wantPhase = control.PhaseDraining
			}
			if after.Phase != wantPhase {
				t.Fatalf("%s: preempted start changed phase to %q", mode, after.Phase)
			}
		})
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

func TestArmedDeadlineSurvivesRestartAndFiresAfterReopen(t *testing.T) {
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
	snap, err := s.ReplaceCatalog(ctx, "", control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "text", Label: "Text", Adapter: "systemd", Unit: "text.service", Cgroup: "/user/text", HealthURL: "http://localhost:9000"}}})
	if err != nil {
		t.Fatal(err)
	}
	state, _ := s.State(ctx)
	state.Phase = control.PhaseStable
	state.DesiredWorkload = control.WorkloadText
	state.ActiveWorkload = control.WorkloadText
	state.Admission = control.AdmissionOpen
	state, err = s.UpdateState(ctx, state.Version, state)
	if err != nil {
		t.Fatal(err)
	}
	settings, _ := s.Settings(ctx)
	e := control.SettingsPrecondition{
		Incarnation: state.LeaseFence.Incarnation, Version: state.Version, Owner: state.Owner,
		ConfigurationRevision: snap.Revision, SettingsRevision: settings.SettingsRevision,
	}
	committed, err := s.SetIdlePolicy(ctx, e, control.IdlePolicy{TimeoutMinutes: 5}, true)
	if err != nil {
		t.Fatal(err)
	}
	deadline := policyEpoch.Add(5 * time.Minute)
	if err := s.ArmIdleDeadline(ctx, committed.SettingsRevision, deadline, policyEpoch.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen past the deadline: the armed deadline must still be honored.
	now = deadline.Add(time.Minute)
	reopened, err := open(ctx, path, clock, uuid)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	settings, err = reopened.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settings.ArmedDeadline == nil || !settings.ArmedDeadline.Equal(deadline) {
		t.Fatalf("armed deadline lost across restart: %#v", settings.ArmedDeadline)
	}
	started, err := reopened.StartIdleTransition(ctx, deadline, idleDrain(t, reopened, "idle-after-restart"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if started.Phase != control.PhaseDraining || started.DesiredWorkload != control.WorkloadIdle {
		t.Fatalf("restart fire = %+v", started)
	}
}

func TestWritePathsDisarmArmedDeadline(t *testing.T) {
	rearm := func(t *testing.T, s *Store) time.Time {
		t.Helper()
		settings, err := s.Settings(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		deadline := policyEpoch.Add(5 * time.Minute)
		if err := s.ArmIdleDeadline(context.Background(), settings.SettingsRevision, deadline, policyEpoch.Add(4*time.Minute)); err != nil {
			t.Fatal(err)
		}
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
		if _, err := s.SetIdlePolicy(context.Background(), e, control.IdlePolicy{TimeoutMinutes: 30}, true); err != nil {
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

func TestAdmitAndIdleStartAreMutuallyExclusiveInEitherOrder(t *testing.T) {
	t.Run("admit first preempts idle", func(t *testing.T) {
		s, now, deadline := armedFixture(t)
		ctx := context.Background()
		state, _ := s.State(ctx)
		*now = deadline
		if err := s.AdmitWork(ctx, "req", "job", control.WorkloadText, state.LeaseFence); err != nil {
			t.Fatal(err)
		}
		if _, err := s.StartIdleTransition(ctx, deadline, idleDrain(t, s, "idle-1"), nil); !errors.Is(err, ErrPolicyPreempted) {
			t.Fatalf("idle after admit: %v", err)
		}
	})
	t.Run("idle first closes admission", func(t *testing.T) {
		s, now, deadline := armedFixture(t)
		ctx := context.Background()
		*now = deadline
		if _, err := s.StartIdleTransition(ctx, deadline, idleDrain(t, s, "idle-1"), nil); err != nil {
			t.Fatal(err)
		}
		fence := idleDrainFence(t, s)
		if err := s.AdmitWork(ctx, "req", "job", control.WorkloadText, fence); !errors.Is(err, ErrAdmissionClosed) {
			t.Fatalf("admit after idle start: %v", err)
		}
	})
}

func idleDrainFence(t *testing.T, s *Store) control.Fence {
	t.Helper()
	state, err := s.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return state.LeaseFence
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

// An admission or completion landing between the evaluator's settings read
// and the arm must preempt the arm, so a stale computed deadline is never
// committed.
func TestArmIdleDeadlineRevalidatesActivityAndPendingWork(t *testing.T) {
	newDeadline := func(t *testing.T, s *Store) (fingerprint string, deadline time.Time) {
		t.Helper()
		settings, err := s.Settings(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return settings.SettingsRevision, settings.LastActivityAt.Add(time.Duration(settings.Policy.TimeoutMinutes) * time.Minute)
	}

	t.Run("matching deadline still arms", func(t *testing.T) {
		s, _ := policyFixture(t)
		enablePolicy(t, s)
		fingerprint, deadline := newDeadline(t, s)
		if err := s.ArmIdleDeadline(context.Background(), fingerprint, deadline, policyEpoch.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		settings, _ := s.Settings(context.Background())
		if settings.ArmedDeadline == nil || !settings.ArmedDeadline.Equal(deadline) {
			t.Fatalf("armed = %#v", settings.ArmedDeadline)
		}
	})
	t.Run("admit between read and arm preempts", func(t *testing.T) {
		s, now := policyFixture(t)
		enablePolicy(t, s)
		fingerprint, deadline := newDeadline(t, s)
		*now = now.Add(time.Minute)
		state, _ := s.State(context.Background())
		if err := s.AdmitWork(context.Background(), "req", "job", control.WorkloadText, state.LeaseFence); err != nil {
			t.Fatal(err)
		}
		if err := s.ArmIdleDeadline(context.Background(), fingerprint, deadline, policyEpoch.Add(time.Minute)); !errors.Is(err, ErrPolicyPreempted) {
			t.Fatalf("arm after concurrent admit: %v", err)
		}
		if settings, _ := s.Settings(context.Background()); settings.ArmedDeadline != nil {
			t.Fatalf("stale deadline armed: %v", settings.ArmedDeadline)
		}
	})
	t.Run("finish between read and arm preempts", func(t *testing.T) {
		s, now := policyFixture(t)
		ctx := context.Background()
		state, _ := s.State(ctx)
		// Admit at the seeded activity time so the read's deadline matches.
		if err := s.AdmitWork(ctx, "req", "job", control.WorkloadText, state.LeaseFence); err != nil {
			t.Fatal(err)
		}
		enablePolicy(t, s)
		fingerprint, deadline := newDeadline(t, s)
		*now = now.Add(time.Minute)
		if err := s.FinishWorkFenced(ctx, "req", control.WorkloadText, state.LeaseFence, WorkCompleted); err != nil {
			t.Fatal(err)
		}
		if err := s.ArmIdleDeadline(ctx, fingerprint, deadline, policyEpoch.Add(time.Minute)); !errors.Is(err, ErrPolicyPreempted) {
			t.Fatalf("arm after concurrent finish: %v", err)
		}
		if settings, _ := s.Settings(ctx); settings.ArmedDeadline != nil {
			t.Fatalf("stale deadline armed: %v", settings.ArmedDeadline)
		}
	})
	t.Run("pending work preempts a freshly computed deadline", func(t *testing.T) {
		s, now := policyFixture(t)
		ctx := context.Background()
		enablePolicy(t, s)
		*now = now.Add(time.Minute)
		state, _ := s.State(ctx)
		if err := s.AdmitWork(ctx, "req", "job", control.WorkloadText, state.LeaseFence); err != nil {
			t.Fatal(err)
		}
		// Read after the admission: the deadline matches the current marker,
		// so the pending-work guard is the one under test.
		fingerprint, deadline := newDeadline(t, s)
		if err := s.ArmIdleDeadline(ctx, fingerprint, deadline, policyEpoch.Add(2*time.Minute)); !errors.Is(err, ErrPolicyPreempted) {
			t.Fatalf("arm with pending work: %v", err)
		}
		if settings, _ := s.Settings(ctx); settings.ArmedDeadline != nil {
			t.Fatalf("deadline armed with pending work: %v", settings.ArmedDeadline)
		}
	})
}

// A degraded workload can never fire its deadline, so arming must preempt
// rather than advertise a permanently overdue deadline.
func TestArmIdleDeadlineRejectsDegradedHealth(t *testing.T) {
	s, _ := policyFixture(t)
	ctx := context.Background()
	fingerprint := enablePolicy(t, s)
	state, _ := s.State(ctx)
	state.Health = control.HealthDegraded
	if _, err := s.UpdateState(ctx, state.Version, state); err != nil {
		t.Fatal(err)
	}
	if err := s.ArmIdleDeadline(ctx, fingerprint, policyEpoch.Add(5*time.Minute), policyEpoch); !errors.Is(err, ErrPolicyPreempted) {
		t.Fatalf("arm on degraded workload: %v", err)
	}
	if settings, _ := s.Settings(ctx); settings.ArmedDeadline != nil {
		t.Fatal("degraded workload armed a deadline")
	}
}

// The timer ticks every minute with the policy Off by default; disarming an
// already-disarmed state must be read-only (no updated_at churn, no WAL
// writes), while a real arm is still cleared.
func TestDisarmIdleDeadlineIsReadOnlyWhenAlreadyDisarmed(t *testing.T) {
	s, _ := policyFixture(t)
	ctx := context.Background()
	updatedAt := func() string {
		var v string
		if err := s.db.QueryRowContext(ctx, `SELECT updated_at FROM idle_policy_state WHERE singleton = 1`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	before := updatedAt()
	for i := 0; i < 3; i++ {
		if err := s.DisarmIdleDeadline(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if after := updatedAt(); after != before {
		t.Fatalf("already-disarmed tick wrote state: %q -> %q", before, after)
	}
	// A real arm is still cleared by the next disarm.
	fingerprint := enablePolicy(t, s)
	deadline := policyEpoch.Add(5 * time.Minute)
	if err := s.ArmIdleDeadline(ctx, fingerprint, deadline, policyEpoch.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.DisarmIdleDeadline(ctx); err != nil {
		t.Fatal(err)
	}
	settings, _ := s.Settings(ctx)
	if settings.ArmedDeadline != nil || settings.AttestationAt != nil {
		t.Fatalf("real arm not cleared: %#v", settings)
	}
	// And the next disarm is read-only again.
	before = updatedAt()
	if err := s.DisarmIdleDeadline(ctx); err != nil {
		t.Fatal(err)
	}
	if after := updatedAt(); after != before {
		t.Fatalf("second disarm wrote state: %q -> %q", before, after)
	}
}

// The provider fence is released after rollback on a failed acquisition or
// after commit on success; queued external work cannot get past it while the
// durable transition still permits admission.
func TestStartIdleTransitionHoldsEvidenceFenceThroughCommit(t *testing.T) {
	s, now, deadline := armedFixture(t)
	ctx := context.Background()
	*now = deadline
	var queue sync.Mutex
	mutated := make(chan control.State, 1)
	fenceHeld := make(chan struct{})
	go func() {
		<-fenceHeld
		queue.Lock()
		defer queue.Unlock()
		state, _ := s.State(context.Background())
		mutated <- state
	}()
	acquired, released := 0, 0
	_, err := s.StartIdleTransition(ctx, deadline, idleDrain(t, s, "idle-fenced"), func(context.Context) (func(), error) {
		acquired++
		queue.Lock()
		close(fenceHeld)
		return func() {
			released++
			check, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			state, err := s.State(check)
			if err != nil || state.Phase != control.PhaseDraining || state.Admission != control.AdmissionClosed {
				t.Errorf("fence released before durable drain: state=%+v err=%v", state, err)
			}
			queue.Unlock()
		}, nil
	})
	if err != nil || acquired != 1 || released != 1 {
		t.Fatalf("fenced transition: err=%v acquisitions=%d releases=%d", err, acquired, released)
	}
	select {
	case state := <-mutated:
		if state.Phase != control.PhaseDraining || state.Admission != control.AdmissionClosed {
			t.Fatalf("external queue mutation preceded durable drain: %+v", state)
		}
	case <-time.After(time.Second):
		t.Fatal("external queue mutation remained blocked after commit")
	}
}

// A failed provider acquisition aborts the transition without clearing the
// arm; a provider returning a held fence with an error is cleaned up exactly
// once, and a missing release cannot authorize a commit.
func TestStartIdleTransitionAcquireFailureRollsBack(t *testing.T) {
	s, now, deadline := armedFixture(t)
	ctx := context.Background()
	*now = deadline
	reject := errors.New("evidence generation revoked")
	released := 0
	if _, err := s.StartIdleTransition(ctx, deadline, idleDrain(t, s, "idle-1"), func(context.Context) (func(), error) {
		return func() {
			released++
			check, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			state, err := s.State(check)
			if err != nil || state.Phase != control.PhaseStable {
				t.Errorf("fence released before rollback: state=%+v err=%v", state, err)
			}
		}, reject
	}); !errors.Is(err, reject) || released != 1 {
		t.Fatalf("acquisition error=%v releases=%d", err, released)
	}
	if _, err := s.StartIdleTransition(ctx, deadline, idleDrain(t, s, "idle-2"), func(context.Context) (func(), error) {
		return nil, nil
	}); !errors.Is(err, ErrEvidenceUnavailable) {
		t.Fatalf("missing release: %v", err)
	}
	after, _ := s.State(ctx)
	if after.Phase != control.PhaseStable || after.ActiveWorkload != control.WorkloadText {
		t.Fatalf("failed acquisition committed: %+v", after)
	}
	settings, _ := s.Settings(ctx)
	if settings.ArmedDeadline == nil {
		t.Fatal("failed acquisition disarmed inside aborted transaction")
	}
	if _, err := s.StartIdleTransition(ctx, deadline, idleDrain(t, s, "idle-3"), func(context.Context) (func(), error) {
		return func() { released++ }, nil
	}); err != nil || released != 2 {
		t.Fatalf("successful acquisition: err=%v releases=%d", err, released)
	}
}
