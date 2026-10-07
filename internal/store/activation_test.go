package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

// activationFixture returns an idle, admission-closed, stable, healthy,
// supervisor-owned store plus the matching operator precondition.
func activationFixture(t *testing.T) (*Store, control.OperatorPrecondition) {
	t.Helper()
	s, _ := policyFixture(t)
	ctx := context.Background()
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state.DesiredWorkload = control.WorkloadIdle
	state.ActiveWorkload = control.WorkloadIdle
	state.Admission = control.AdmissionClosed
	state, err = s.UpdateState(ctx, state.Version, state)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := s.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e := control.OperatorPrecondition{
		Incarnation: state.LeaseFence.Incarnation, Version: state.Version,
		Owner: control.OwnerSupervisor, ConfigurationRevision: snap.Revision,
	}
	return s, e
}

func activationTransition(t *testing.T, s *Store, id string) Transition {
	t.Helper()
	current, err := s.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	snap, err := s.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	target := current
	target.DesiredWorkload = control.WorkloadText
	target.ActiveWorkload = control.WorkloadText
	target.Admission = control.AdmissionOpen
	return Transition{
		ID: id, Source: current, Target: target, Previous: current,
		ConfigurationRevision: snap.Revision, Deadline: time.Now().Add(time.Minute),
	}
}

func TestStartActivationTransitionFromIdleStartsDrain(t *testing.T) {
	s, e := activationFixture(t)
	ctx := context.Background()
	if err := s.CheckActivationPrecondition(ctx, e); err != nil {
		t.Fatal(err)
	}
	started, err := s.StartActivationTransition(ctx, e, activationTransition(t, s, "activate-1"))
	if err != nil {
		t.Fatal(err)
	}
	if started.Phase != control.PhaseDraining || started.DesiredWorkload != control.WorkloadText || started.Admission != control.AdmissionClosed {
		t.Fatalf("activation start = %+v", started)
	}
}

func TestActivationDeferredWhileWorkloadActive(t *testing.T) {
	s, _ := policyFixture(t) // stable, open, active text workload
	ctx := context.Background()
	state, _ := s.State(ctx)
	snap, _ := s.Catalog(ctx)
	e := control.OperatorPrecondition{Incarnation: state.LeaseFence.Incarnation, Version: state.Version, Owner: control.OwnerSupervisor, ConfigurationRevision: snap.Revision}
	if err := s.CheckActivationPrecondition(ctx, e); !errors.Is(err, ErrNotIdle) {
		t.Fatalf("precondition: %v", err)
	}
	if _, err := s.StartActivationTransition(ctx, e, activationTransition(t, s, "a")); !errors.Is(err, ErrNotIdle) {
		t.Fatalf("start: %v", err)
	}
	after, _ := s.State(ctx)
	if after.Version != state.Version {
		t.Fatal("deferred activation changed state")
	}
}

func TestActivationDeferredWithUnfinishedWork(t *testing.T) {
	s, e := activationFixture(t)
	ctx := context.Background()
	// Admit work while the workload is still running: move to text, admit, then drain back.
	state, _ := s.State(ctx)
	media := state
	media.DesiredWorkload = control.WorkloadText
	media.ActiveWorkload = control.WorkloadText
	media.Admission = control.AdmissionOpen
	tr := Transition{ID: "up", Source: state, Target: media, Previous: media, ConfigurationRevision: e.ConfigurationRevision, Deadline: time.Now().Add(time.Minute)}
	started, err := s.StartTransition(ctx, state.Version, tr)
	if err != nil {
		t.Fatal(err)
	}
	media.LeaseFence = started.LeaseFence
	if _, err := s.FinishTransition(ctx, tr.ID, "committed", started.Version, media); err != nil {
		t.Fatal(err)
	}
	running, _ := s.State(ctx)
	if err := s.AdmitWork(ctx, "req", "job", control.WorkloadText, running.LeaseFence); err != nil {
		t.Fatal(err)
	}
	idle := running
	idle.DesiredWorkload = control.WorkloadIdle
	idle.ActiveWorkload = control.WorkloadIdle
	idle.Admission = control.AdmissionClosed
	back := Transition{ID: "down", Source: running, Target: idle, Previous: idle, ConfigurationRevision: e.ConfigurationRevision, Deadline: time.Now().Add(time.Minute)}
	started, err = s.StartTransition(ctx, running.Version, back)
	if err != nil {
		t.Fatal(err)
	}
	idle.LeaseFence = started.LeaseFence
	committed, err := s.FinishTransition(ctx, back.ID, "committed", started.Version, idle)
	if err != nil {
		t.Fatal(err)
	}
	e.Incarnation = committed.LeaseFence.Incarnation
	e.Version = committed.Version
	if err := s.CheckActivationPrecondition(ctx, e); !errors.Is(err, ErrUnresolvedWork) {
		t.Fatalf("precondition: %v", err)
	}
	if _, err := s.StartActivationTransition(ctx, e, activationTransition(t, s, "a")); !errors.Is(err, ErrUnresolvedWork) {
		t.Fatalf("start: %v", err)
	}
}

func TestActivationBusyDuringLiveTransition(t *testing.T) {
	s, e := activationFixture(t)
	ctx := context.Background()
	tr := activationTransition(t, s, "first")
	if _, err := s.StartActivationTransition(ctx, e, tr); err != nil {
		t.Fatal(err)
	}
	// A second activation while the first is draining reports busy, never a
	// shadowed stability error, even with a stale expected version.
	if _, err := s.StartActivationTransition(ctx, e, activationTransition(t, s, "second")); !errors.Is(err, ErrTransitionRunning) {
		t.Fatalf("second activation: %v", err)
	}
}

func TestActivationRejectsWrongOwnerStaleAndUnhealthy(t *testing.T) {
	s, e := activationFixture(t)
	ctx := context.Background()
	wrongOwner := e
	wrongOwner.Owner = control.OwnerUser
	if _, err := s.StartActivationTransition(ctx, wrongOwner, activationTransition(t, s, "a")); !errors.Is(err, ErrWrongOwner) {
		t.Fatalf("user owner: %v", err)
	}
	stale := e
	stale.Version++
	if _, err := s.StartActivationTransition(ctx, stale, activationTransition(t, s, "a")); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale version: %v", err)
	}
	state, _ := s.State(ctx)
	state.Health = control.HealthDegraded
	state, err := s.UpdateState(ctx, state.Version, state)
	if err != nil {
		t.Fatal(err)
	}
	e.Version = state.Version
	if _, err := s.StartActivationTransition(ctx, e, activationTransition(t, s, "a")); !errors.Is(err, ErrUnstableState) {
		t.Fatalf("degraded health: %v", err)
	}
}
