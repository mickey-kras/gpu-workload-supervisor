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
