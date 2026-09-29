package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestInitialStateIsRecoverySafe(t *testing.T) {
	store := testStore(t)
	state, err := store.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Owner != control.OwnerSupervisor {
		t.Fatalf("owner = %q", state.Owner)
	}
	if state.DesiredWorkload != control.WorkloadIdle {
		t.Fatalf("desired = %q", state.DesiredWorkload)
	}
	if state.ActiveWorkload != control.WorkloadUnknown {
		t.Fatalf("active = %q", state.ActiveWorkload)
	}
	if state.Phase != control.PhaseReconciling {
		t.Fatalf("phase = %q", state.Phase)
	}
	if state.Admission != control.AdmissionClosed {
		t.Fatalf("admission = %q", state.Admission)
	}
}

func TestStateSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	first, err := open(context.Background(), path, fixedClock(), fixedUUID("11111111-1111-4111-8111-111111111111"))
	if err != nil {
		t.Fatal(err)
	}
	original, err := first.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := open(context.Background(), path, fixedClock(), fixedUUID("22222222-2222-4222-8222-222222222222"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { second.Close() })
	reopened, err := second.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if reopened.LeaseFence != original.LeaseFence {
		t.Fatalf("fence changed: %#v != %#v", reopened.LeaseFence, original.LeaseFence)
	}
}

func TestFenceRotationRejectsStaleWork(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	state, _ := store.State(ctx)
	state.Phase = control.PhaseStable
	state.Admission = control.AdmissionOpen
	opened, err := store.UpdateState(ctx, state.Version, state)
	if err != nil {
		t.Fatal(err)
	}
	oldFence := opened.LeaseFence
	rotated, err := store.RotateFenceAndCloseAdmission(ctx, opened.Version)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.LeaseFence.Epoch != oldFence.Epoch+1 {
		t.Fatalf("epoch = %d", rotated.LeaseFence.Epoch)
	}
	if err := store.RegisterWork(ctx, "request-1", "job-1", control.WorkloadText, oldFence); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("register stale work: %v", err)
	}
}

func TestIncarnationRotationIsRecoverySafe(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	before, _ := store.State(ctx)
	after, err := store.RotateIncarnation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.LeaseFence.Incarnation == before.LeaseFence.Incarnation {
		t.Fatal("incarnation did not change")
	}
	if after.Phase != control.PhaseReconciling || after.Admission != control.AdmissionClosed {
		t.Fatalf("unsafe state: %#v", after)
	}
}

func TestTransitionJournalOrdersIntentBeforeObservation(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	state, _ := store.State(ctx)
	tr := Transition{ID: "transition-1", Fence: state.LeaseFence, Source: state, Target: state,
		Previous: state, Initiator: "test", Phase: control.PhaseUnloading,
		Deadline: time.Date(2026, 9, 29, 12, 5, 0, 0, time.UTC)}
	if err := store.BeginTransition(ctx, tr); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendTransitionEvent(ctx, TransitionEvent{TransitionID: tr.ID, Phase: control.PhaseUnloading, Kind: "intent", Action: "stop text"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendTransitionEvent(ctx, TransitionEvent{TransitionID: tr.ID, Phase: control.PhaseUnloading, Kind: "observation", Action: "stop text", Outcome: "stopped"}); err != nil {
		t.Fatal(err)
	}
	events, err := store.TransitionEvents(ctx, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Kind != "intent" || events[1].Kind != "observation" {
		t.Fatalf("events = %#v", events)
	}
}

func testStore(t *testing.T) *Store {
	t.Helper()
	ids := []string{"11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"}
	index := 0
	uuid := func() (string, error) {
		value := ids[index]
		if index < len(ids)-1 {
			index++
		}
		return value, nil
	}
	store, err := open(context.Background(), filepath.Join(t.TempDir(), "state.db"), fixedClock(), uuid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func fixedClock() Clock {
	return func() time.Time { return time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC) }
}
func fixedUUID(value string) func() (string, error) {
	return func() (string, error) { return value, nil }
}

func TestMigratesV1WithoutChangingPersistedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	createV1Database(t, path, false)
	stateStore, err := open(context.Background(), path, fixedClock(), fixedUUID("22222222-2222-4222-8222-222222222222"))
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()
	state, err := stateStore.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.LeaseFence.Incarnation != "11111111-1111-4111-8111-111111111111" || state.Version != 7 {
		t.Fatalf("persisted state changed: %#v", state)
	}
	var version, work, transitions, events int
	if err := stateStore.db.QueryRow("SELECT MAX(version) FROM schema_migrations").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := stateStore.db.QueryRow("SELECT COUNT(*) FROM registered_work").Scan(&work); err != nil {
		t.Fatal(err)
	}
	if err := stateStore.db.QueryRow("SELECT COUNT(*) FROM transitions").Scan(&transitions); err != nil {
		t.Fatal(err)
	}
	if err := stateStore.db.QueryRow("SELECT COUNT(*) FROM transition_events").Scan(&events); err != nil {
		t.Fatal(err)
	}
	if version != 3 || work != 1 || transitions != 1 || events != 1 {
		t.Fatalf("migration result version=%d work=%d transitions=%d events=%d", version, work, transitions, events)
	}
}

func TestFailedV2MigrationRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	createV1Database(t, path, true)
	if _, err := open(context.Background(), path, fixedClock(), fixedUUID("22222222-2222-4222-8222-222222222222")); err == nil {
		t.Fatal("expected migration failure")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, stateRows int
	if err := db.QueryRow("SELECT MAX(version) FROM schema_migrations").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM control_state").Scan(&stateRows); err != nil {
		t.Fatal(err)
	}
	if version != 1 || stateRows != 1 {
		t.Fatalf("failed migration changed database: version=%d states=%d", version, stateRows)
	}
}

func createV1Database(t *testing.T, path string, conflict bool) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(schemaV1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO schema_migrations(version, applied_at) VALUES (1, '2026-09-29T12:00:00Z')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO control_state
		(singleton, owner, desired_workload, active_workload, phase, health, admission,
		 lease_incarnation, lease_epoch, version, updated_at)
		VALUES (1, 'supervisor', 'text', 'text', 'stable', 'healthy', 'open', ?, 4, 7, ?)`,
		"11111111-1111-4111-8111-111111111111", "2026-09-29T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO registered_work
		(request_id, job_id, lease_incarnation, lease_epoch, registered_at)
		VALUES ('request-1', 'job-1', ?, 4, ?)`,
		"11111111-1111-4111-8111-111111111111", "2026-09-29T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO transitions
		(transition_id, lease_incarnation, lease_epoch, source_state, target_state,
		 previous_state, initiator, phase, deadline, status, created_at, updated_at)
		VALUES ('transition-1', ?, 4, '{}', '{}', '{}', 'test', 'stable', ?, 'committed', ?, ?)`,
		"11111111-1111-4111-8111-111111111111", "2026-09-29T12:05:00Z",
		"2026-09-29T12:00:00Z", "2026-09-29T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO transition_events
		(transition_id, phase, kind, action, outcome, created_at)
		VALUES ('transition-1', 'stable', 'observation', 'migrated', 'ok', ?)`,
		"2026-09-29T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if conflict {
		if _, err := db.Exec("CREATE TABLE transition_work (unexpected INTEGER)"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRegisterWorkRequiresMatchingStableWorkload(t *testing.T) {
	stateStore := testStore(t)
	ctx := context.Background()
	state, err := stateStore.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state.DesiredWorkload = control.WorkloadText
	state.ActiveWorkload = control.WorkloadText
	state.Phase = control.PhaseStable
	state.Health = control.HealthHealthy
	state.Admission = control.AdmissionOpen
	state, err = stateStore.UpdateState(ctx, state.Version, state)
	if err != nil {
		t.Fatal(err)
	}
	if err := stateStore.RegisterWork(ctx, "media-request", "job-1", control.WorkloadMedia, state.LeaseFence); !errors.Is(err, ErrWorkloadMismatch) {
		t.Fatalf("register mismatched work: %v", err)
	}
	if err := stateStore.RegisterWork(ctx, "text-request", "job-1", control.WorkloadText, state.LeaseFence); err != nil {
		t.Fatal(err)
	}
}
