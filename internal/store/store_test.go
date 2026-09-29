package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestInitialStateIsRecoverySafe(t *testing.T) {
	store := testStore(t)
	state, err := store.State(context.Background())
	if err != nil { t.Fatal(err) }
	if state.Owner != control.OwnerSupervisor { t.Fatalf("owner = %q", state.Owner) }
	if state.DesiredWorkload != control.WorkloadIdle { t.Fatalf("desired = %q", state.DesiredWorkload) }
	if state.ActiveWorkload != control.WorkloadUnknown { t.Fatalf("active = %q", state.ActiveWorkload) }
	if state.Phase != control.PhaseReconciling { t.Fatalf("phase = %q", state.Phase) }
	if state.Admission != control.AdmissionClosed { t.Fatalf("admission = %q", state.Admission) }
}

func TestStateSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	first, err := open(context.Background(), path, fixedClock(), fixedUUID("11111111-1111-4111-8111-111111111111"))
	if err != nil { t.Fatal(err) }
	original, err := first.State(context.Background())
	if err != nil { t.Fatal(err) }
	if err := first.Close(); err != nil { t.Fatal(err) }
	second, err := open(context.Background(), path, fixedClock(), fixedUUID("22222222-2222-4222-8222-222222222222"))
	if err != nil { t.Fatal(err) }
	t.Cleanup(func() { second.Close() })
	reopened, err := second.State(context.Background())
	if err != nil { t.Fatal(err) }
	if reopened.LeaseFence != original.LeaseFence { t.Fatalf("fence changed: %#v != %#v", reopened.LeaseFence, original.LeaseFence) }
}

func TestFenceRotationRejectsStaleWork(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	state, _ := store.State(ctx)
	state.Phase = control.PhaseStable
	state.Admission = control.AdmissionOpen
	opened, err := store.UpdateState(ctx, state.Version, state)
	if err != nil { t.Fatal(err) }
	oldFence := opened.LeaseFence
	rotated, err := store.RotateFenceAndCloseAdmission(ctx, opened.Version)
	if err != nil { t.Fatal(err) }
	if rotated.LeaseFence.Epoch != oldFence.Epoch+1 { t.Fatalf("epoch = %d", rotated.LeaseFence.Epoch) }
	if err := store.RegisterWork(ctx, "request-1", "job-1", oldFence); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("register stale work: %v", err)
	}
}

func TestIncarnationRotationIsRecoverySafe(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	before, _ := store.State(ctx)
	after, err := store.RotateIncarnation(ctx)
	if err != nil { t.Fatal(err) }
	if after.LeaseFence.Incarnation == before.LeaseFence.Incarnation { t.Fatal("incarnation did not change") }
	if after.Phase != control.PhaseReconciling || after.Admission != control.AdmissionClosed { t.Fatalf("unsafe state: %#v", after) }
}

func TestTransitionJournalOrdersIntentBeforeObservation(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	state, _ := store.State(ctx)
	tr := Transition{ID: "transition-1", Fence: state.LeaseFence, Source: state, Target: state,
		Previous: state, Initiator: "test", Phase: control.PhaseUnloading,
		Deadline: time.Date(2026, 9, 29, 12, 5, 0, 0, time.UTC)}
	if err := store.BeginTransition(ctx, tr); err != nil { t.Fatal(err) }
	if err := store.AppendTransitionEvent(ctx, TransitionEvent{TransitionID: tr.ID, Phase: control.PhaseUnloading, Kind: "intent", Action: "stop text"}); err != nil { t.Fatal(err) }
	if err := store.AppendTransitionEvent(ctx, TransitionEvent{TransitionID: tr.ID, Phase: control.PhaseUnloading, Kind: "observation", Action: "stop text", Outcome: "stopped"}); err != nil { t.Fatal(err) }
	events, err := store.TransitionEvents(ctx, tr.ID)
	if err != nil { t.Fatal(err) }
	if len(events) != 2 || events[0].Kind != "intent" || events[1].Kind != "observation" { t.Fatalf("events = %#v", events) }
}

func testStore(t *testing.T) *Store {
	t.Helper()
	ids := []string{"11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"}
	index := 0
	uuid := func() (string, error) { value := ids[index]; if index < len(ids)-1 { index++ }; return value, nil }
	store, err := open(context.Background(), filepath.Join(t.TempDir(), "state.db"), fixedClock(), uuid)
	if err != nil { t.Fatal(err) }
	t.Cleanup(func() { store.Close() })
	return store
}

func fixedClock() Clock {
	return func() time.Time { return time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC) }
}
func fixedUUID(value string) func() (string, error) {
	return func() (string, error) { return value, nil }
}
