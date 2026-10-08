package store

import (
	"context"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"testing"
	"time"
)

func startTemporaryFixture(t *testing.T) (*Store, control.State, control.TemporaryDiscoverySession) {
	t.Helper()
	s, e := activationFixture(t)
	ctx := context.Background()
	current, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	v := control.TemporaryDiscoverySession{ID: "temporary-one", Token: "private-token", Status: "starting", PriorStopped: true, Candidate: control.TemporaryDiscoveryCandidate{Unit: "ollama.service", Endpoint: "http://127.0.0.1:11434"}}
	tr := Transition{ID: v.ID, Source: current, Target: current, Previous: current, Initiator: "temporary-native-discovery", Deadline: time.Now().Add(time.Minute), ConfigurationRevision: e.ConfigurationRevision}
	next, err := s.StartTemporaryDiscovery(ctx, e, tr, v)
	if err != nil {
		t.Fatal(err)
	}
	record, err := s.TemporaryDiscoveryStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return s, next, *record
}
func TestTemporarySessionClosesFenceAndBlocksOtherControlPaths(t *testing.T) {
	s, state, v := startTemporaryFixture(t)
	defer s.Close()
	ctx := context.Background()
	if state.Admission != control.AdmissionClosed || v.Fence != state.LeaseFence || v.Version != state.Version || !v.PriorStopped {
		t.Fatalf("unsafe session %+v %+v", state, v)
	}
	snap, err := s.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	operations := map[string]func() error{
		"catalog": func() error { _, err := s.ReplaceCatalog(ctx, snap.Revision, snap.Catalog); return err },
		"recover": func() error { _, err := s.Recover(ctx, state.Version, state, "test"); return err },
		"update":  func() error { _, err := s.UpdateState(ctx, state.Version, state); return err },
		"rotate":  func() error { _, err := s.RotateFenceAndCloseAdmission(ctx, state.Version); return err },
		"finish":  func() error { _, err := s.FinishTransition(ctx, v.ID, "committed", state.Version, state); return err },
		"begin": func() error {
			_, err := s.StartTransition(ctx, state.Version, Transition{ID: "blocked-transition", Target: state, Previous: state, Deadline: time.Now().Add(time.Minute), ConfigurationRevision: snap.Revision})
			return err
		},
		"incarnation": func() error { _, err := s.RotateIncarnation(ctx); return err },
		"phase": func() error {
			_, err := s.SetTransitionPhase(ctx, v.ID, state.Version, control.PhaseStable)
			return err
		},
		"ownership": func() error {
			return s.CheckOperatorPrecondition(ctx, control.OperatorPrecondition{Incarnation: state.LeaseFence.Incarnation, Version: state.Version, Owner: state.Owner, ConfigurationRevision: snap.Revision})
		},
	}
	for name, operation := range operations {
		if err := operation(); !errors.Is(err, ErrTemporaryCleanupRequired) {
			t.Errorf("%s=%v", name, err)
		}
	}
	after, err := s.State(ctx)
	if err != nil || after.Version != state.Version || after.LeaseFence != state.LeaseFence {
		t.Fatalf("blocked operation changed state: %+v %v", after, err)
	}
	if _, err = s.FinishTemporaryDiscovery(ctx, v.ID, "wrong"); !errors.Is(err, ErrStaleFence) {
		t.Fatal(err)
	}
	if _, err = s.FinishTemporaryDiscovery(ctx, v.ID, v.Token); err != nil {
		t.Fatal(err)
	}
	done, err := s.TemporaryDiscoveryStatus(ctx)
	if err != nil || done.Status != "completed" {
		t.Fatalf("%+v %v", done, err)
	}
	after, err = s.State(ctx)
	if err != nil || after.Admission != control.AdmissionClosed || after.Phase != control.PhaseStable || after.ActiveWorkload != control.WorkloadIdle {
		t.Fatalf("%+v %v", after, err)
	}
}
func TestTemporarySessionInvocationCannotBeReplaced(t *testing.T) {
	s, _, v := startTemporaryFixture(t)
	defer s.Close()
	ctx := context.Background()
	launch := control.TemporaryDiscoveryLaunchEvidence{InvocationID: "abc", JobID: "32", ActivationTimestamp: "13"}
	if err := s.RecordTemporaryLaunch(ctx, v.ID, v.Token, launch); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTemporaryDiscovery(ctx, v.ID, v.Token, "abc", "running", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTemporaryDiscovery(ctx, v.ID, v.Token, "other", "cleanup_required", ""); !errors.Is(err, ErrStaleFence) {
		t.Fatal(err)
	}
	if err := s.RecordTemporaryLaunch(ctx, v.ID, v.Token, launch); !errors.Is(err, ErrStaleFence) {
		t.Fatal(err)
	}
}
