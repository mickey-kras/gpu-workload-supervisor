package store

import (
	"context"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"testing"
	"time"
)

func TestOperatorStartAtomic(t *testing.T) {
	for _, mode := range []string{"ok", "owner", "incarnation", "version", "revision"} {
		t.Run(mode, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			s.db.Exec(`CREATE TABLE IF NOT EXISTS workload_catalog (singleton INTEGER PRIMARY KEY, revision TEXT NOT NULL, catalog BLOB NOT NULL)`)
			s.db.Exec(`INSERT INTO workload_catalog VALUES(1,'rev','{}')`)
			state, _ := s.State(ctx)
			state.Phase = control.PhaseStable
			state.ActiveWorkload = control.WorkloadIdle
			state.Owner = control.OwnerUser
			state, _ = s.UpdateState(ctx, state.Version, state)
			e := control.OperatorPrecondition{Incarnation: state.LeaseFence.Incarnation, Version: state.Version, Owner: state.Owner, ConfigurationRevision: "rev"}
			want := error(nil)
			switch mode {
			case "owner":
				e.Owner = control.OwnerSupervisor
				want = ErrWrongOwner
			case "incarnation":
				e.Incarnation = "restored"
				want = ErrVersionConflict
			case "version":
				e.Version++
				want = ErrVersionConflict
			case "revision":
				e.ConfigurationRevision = "new"
				want = ErrConfigurationConflict
			}
			_, err := s.StartOperatorTransition(ctx, e, Transition{ID: "operator", Target: state, Previous: state, ConfigurationRevision: "rev", Deadline: time.Now().Add(time.Minute)})
			if !errors.Is(err, want) {
				t.Fatalf("%v want %v", err, want)
			}
			if want != nil {
				after, _ := s.State(ctx)
				if after != state {
					t.Fatal("rejected request mutated state")
				}
			}
		})
	}
}

func TestCheckOperatorPreconditionRejectsStaleAndUnstableSources(t *testing.T) {
	for _, mode := range []string{"ok", "missing catalog", "stale incarnation", "stale version", "wrong owner", "stale revision", "unstable phase", "health error", "transition running"} {
		t.Run(mode, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			var revision string
			if mode != "missing catalog" {
				snap, err := s.ReplaceCatalog(ctx, "", control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "text", Label: "Text", Adapter: "systemd", Unit: "text.service", Cgroup: "/user/text", HealthURL: "http://localhost:9000"}}})
				if err != nil {
					t.Fatal(err)
				}
				revision = snap.Revision
			}
			state, err := s.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			state.Phase = control.PhaseStable
			state, err = s.UpdateState(ctx, state.Version, state)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "unstable phase" {
				unstable := state
				unstable.Phase = control.PhaseDraining
				if _, err := s.UpdateState(ctx, state.Version, unstable); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "health error" {
				unhealthy := state
				unhealthy.Health = control.HealthError
				if _, err := s.UpdateState(ctx, state.Version, unhealthy); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "transition running" {
				if err := s.BeginTransition(ctx, Transition{ID: "running", Fence: state.LeaseFence, Source: state, Target: state, Previous: state, Deadline: time.Now().Add(time.Minute)}); err != nil {
					t.Fatal(err)
				}
			}
			current, err := s.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			e := control.OperatorPrecondition{Incarnation: current.LeaseFence.Incarnation, Version: current.Version, Owner: current.Owner, ConfigurationRevision: revision}
			want := error(nil)
			switch mode {
			case "missing catalog", "stale revision":
				e.ConfigurationRevision = "other"
				want = ErrConfigurationConflict
			case "stale incarnation":
				e.Incarnation = "restored"
				want = ErrVersionConflict
			case "stale version":
				e.Version++
				want = ErrVersionConflict
			case "wrong owner":
				e.Owner = control.OwnerUser
				want = ErrWrongOwner
			case "unstable phase", "health error":
				want = ErrUnstableState
			case "transition running":
				want = ErrTransitionRunning
			}
			if err := s.CheckOperatorPrecondition(ctx, e); !errors.Is(err, want) {
				t.Fatalf("%v want %v", err, want)
			}
		})
	}
}

func TestCheckOperatorPreconditionPropagatesStorageFailure(t *testing.T) {
	s := testStore(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckOperatorPrecondition(context.Background(), control.OperatorPrecondition{}); err == nil {
		t.Fatal("closed store accepted precondition check")
	}
}
