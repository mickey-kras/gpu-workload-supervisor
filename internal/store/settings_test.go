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

// settingsStore opens a store with an unlimited uuid sequence so every
// settings write rotates to a distinct opaque revision.
func settingsStore(t *testing.T) *Store {
	t.Helper()
	var index atomic.Int64
	uuid := func() (string, error) {
		return fmt.Sprintf("00000000-0000-4000-8000-%012d", index.Add(1)), nil
	}
	s, err := open(context.Background(), filepath.Join(t.TempDir(), "state.db"), fixedClock(), uuid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// stableSettingsFixture moves the seeded store into a stable supervisor-owned
// state with an accepted catalog, the precondition baseline for settings writes.
func stableSettingsFixture(t *testing.T, s *Store) (control.SettingsPrecondition, string) {
	t.Helper()
	ctx := context.Background()
	snap, err := s.ReplaceCatalog(ctx, "", control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "text", Label: "Text", Adapter: "systemd", Unit: "text.service", Cgroup: "/user/text", HealthURL: "http://localhost:9000"}}})
	if err != nil {
		t.Fatal(err)
	}
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state.Phase = control.PhaseStable
	state.ActiveWorkload = control.WorkloadIdle
	if _, err := s.UpdateState(ctx, state.Version, state); err != nil {
		t.Fatal(err)
	}
	current, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := s.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return control.SettingsPrecondition{
		Incarnation: current.LeaseFence.Incarnation, Version: current.Version,
		Owner: current.Owner, ConfigurationRevision: snap.Revision,
		SettingsRevision: settings.SettingsRevision,
	}, settings.SettingsRevision
}

func TestSettingsSeededOffWithOpaqueRevision(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	settings, err := s.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Policy.TimeoutMinutes != 0 {
		t.Fatalf("seeded policy = %#v, want Off", settings.Policy)
	}
	if settings.SettingsRevision == "" {
		t.Fatal("seeded settings revision is empty")
	}
	if settings.LastActivityAt == nil || !settings.LastActivityAt.Equal(fixedClock()()) {
		t.Fatalf("seeded last activity = %v, want migration time", settings.LastActivityAt)
	}
	if settings.ArmedDeadline != nil || settings.AttestationAt != nil {
		t.Fatalf("seeded policy state armed: %#v", settings)
	}
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.IdlePolicy.TimeoutMinutes != 0 {
		t.Fatalf("state read model policy = %#v, want Off", state.IdlePolicy)
	}
}

func TestSetIdlePolicyCommitsAndRotatesRevisionWithoutTouchingControlState(t *testing.T) {
	s := settingsStore(t)
	ctx := context.Background()
	e, seedRevision := stableSettingsFixture(t, s)
	before, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	committed, err := s.SetIdlePolicy(ctx, e, control.IdlePolicy{TimeoutMinutes: 60})
	if err != nil {
		t.Fatal(err)
	}
	if committed.Policy.TimeoutMinutes != 60 {
		t.Fatalf("committed policy = %#v", committed.Policy)
	}
	if committed.SettingsRevision == seedRevision {
		t.Fatal("settings revision was not rotated")
	}
	after, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != before.Version || after.LeaseFence != before.LeaseFence {
		t.Fatalf("settings write moved control state: before=%+v after=%+v", before, after)
	}
	if after.IdlePolicy.TimeoutMinutes != 60 {
		t.Fatalf("state read model policy = %#v", after.IdlePolicy)
	}
}

func TestSetIdlePolicyRejectsOutOfBoundsTimeout(t *testing.T) {
	for _, timeout := range []int{-1, 1, 4, 1441, 1 << 40} {
		t.Run(fmt.Sprintf("%d", timeout), func(t *testing.T) {
			s := settingsStore(t)
			ctx := context.Background()
			e, seedRevision := stableSettingsFixture(t, s)
			if _, err := s.SetIdlePolicy(ctx, e, control.IdlePolicy{TimeoutMinutes: timeout}); !errors.Is(err, ErrInvalidIdleTimeout) {
				t.Fatalf("timeout %d: %v", timeout, err)
			}
			settings, err := s.Settings(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if settings.Policy.TimeoutMinutes != 0 || settings.SettingsRevision != seedRevision {
				t.Fatalf("rejected write changed settings: %#v", settings)
			}
		})
	}
}

func TestSetIdlePolicyRejectsStalePreconditionsWithoutEffects(t *testing.T) {
	for _, mode := range []string{"settings revision", "empty settings revision", "incarnation", "version", "owner", "catalog revision", "unstable phase", "health error", "transition running"} {
		t.Run(mode, func(t *testing.T) {
			s := settingsStore(t)
			ctx := context.Background()
			e, _ := stableSettingsFixture(t, s)
			want := error(nil)
			switch mode {
			case "settings revision":
				e.SettingsRevision = "stale"
				want = ErrSettingsConflict
			case "empty settings revision":
				e.SettingsRevision = ""
				want = ErrSettingsConflict
			case "incarnation":
				e.Incarnation = "restored"
				want = ErrVersionConflict
			case "version":
				e.Version++
				want = ErrVersionConflict
			case "owner":
				e.Owner = control.OwnerUser
				want = ErrWrongOwner
			case "catalog revision":
				e.ConfigurationRevision = "other"
				want = ErrConfigurationConflict
			case "unstable phase", "health error":
				current, err := s.State(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "unstable phase" {
					current.Phase = control.PhaseDraining
				} else {
					current.Health = control.HealthError
				}
				if _, err := s.UpdateState(ctx, current.Version, current); err != nil {
					t.Fatal(err)
				}
				e.Version = current.Version + 1
				want = ErrUnstableState
			case "transition running":
				current, err := s.State(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if err := s.BeginTransition(ctx, Transition{ID: "running", Fence: current.LeaseFence, Source: current, Target: current, Previous: current, Deadline: time.Now().Add(time.Minute)}); err != nil {
					t.Fatal(err)
				}
				want = ErrTransitionRunning
			}
			if _, err := s.SetIdlePolicy(ctx, e, control.IdlePolicy{TimeoutMinutes: 30}); !errors.Is(err, want) {
				t.Fatalf("%v want %v", err, want)
			}
			settings, err := s.Settings(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if settings.Policy.TimeoutMinutes != 0 {
				t.Fatalf("rejected write changed policy: %#v", settings.Policy)
			}
		})
	}
}

func TestSetIdlePolicyClearsArmedDeadlineAndAttestation(t *testing.T) {
	s := settingsStore(t)
	ctx := context.Background()
	e, _ := stableSettingsFixture(t, s)
	armed := formatTime(fixedClock()().Add(time.Hour))
	if _, err := s.db.ExecContext(ctx, `UPDATE idle_policy_state SET armed_deadline = ?, attestation_at = ? WHERE singleton = 1`, armed, armed); err != nil {
		t.Fatal(err)
	}
	committed, err := s.SetIdlePolicy(ctx, e, control.IdlePolicy{TimeoutMinutes: 15})
	if err != nil {
		t.Fatal(err)
	}
	if committed.ArmedDeadline != nil || committed.AttestationAt != nil {
		t.Fatalf("settings write kept armed deadline: %#v", committed)
	}
}

func TestSetIdlePolicyConcurrentWritersHaveSingleWinner(t *testing.T) {
	s := settingsStore(t)
	ctx := context.Background()
	e, _ := stableSettingsFixture(t, s)
	var wg sync.WaitGroup
	codes := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(timeout int) {
			defer wg.Done()
			_, err := s.SetIdlePolicy(ctx, e, control.IdlePolicy{TimeoutMinutes: timeout})
			codes <- err
		}(30 + i)
	}
	wg.Wait()
	close(codes)
	var wins, conflicts int
	for err := range codes {
		if err == nil {
			wins++
		} else if errors.Is(err, ErrSettingsConflict) {
			conflicts++
		} else {
			t.Fatalf("unexpected outcome: %v", err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
	settings, err := s.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settings.SettingsRevision == e.SettingsRevision {
		t.Fatal("winner did not rotate the settings revision")
	}
}

func TestSetIdlePolicyToOffDisarmsDeadline(t *testing.T) {
	s := settingsStore(t)
	ctx := context.Background()
	e, _ := stableSettingsFixture(t, s)
	enabled, err := s.SetIdlePolicy(ctx, e, control.IdlePolicy{TimeoutMinutes: 45})
	if err != nil {
		t.Fatal(err)
	}
	armed := formatTime(fixedClock()().Add(time.Hour))
	if _, err := s.db.ExecContext(ctx, `UPDATE idle_policy_state SET armed_deadline = ? WHERE singleton = 1`, armed); err != nil {
		t.Fatal(err)
	}
	e.Version = mustState(t, s).Version
	e.SettingsRevision = enabled.SettingsRevision
	disabled, err := s.SetIdlePolicy(ctx, e, control.IdlePolicy{TimeoutMinutes: 0})
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Policy.TimeoutMinutes != 0 || disabled.ArmedDeadline != nil {
		t.Fatalf("disable result = %#v", disabled)
	}
}

func mustState(t *testing.T, s *Store) control.State {
	t.Helper()
	state, err := s.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestSettingsFailClosedOnClosedStore(t *testing.T) {
	s := settingsStore(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Settings(context.Background()); err == nil {
		t.Fatal("closed store served settings")
	}
	if _, err := s.SetIdlePolicy(context.Background(), control.SettingsPrecondition{}, control.IdlePolicy{}); err == nil {
		t.Fatal("closed store accepted a settings write")
	}
}
