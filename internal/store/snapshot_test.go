package store

import (
	"context"
	"database/sql"
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

// QueryRowContext has stepped to the first SQLite row before returning, so a
// second Store can commit in WAL mode after the reader establishes its snapshot.
// Keep callback errors until Scan releases the reader's sole connection.
type snapshotQuerier struct {
	db     *sql.DB
	commit func(context.Context) error
	err    error
}

func (q *snapshotQuerier) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	row := q.db.QueryRowContext(ctx, query, args...)
	if q.commit != nil {
		commit := q.commit
		q.commit = nil
		writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		q.err = commit(writeCtx)
	}
	return row
}

func snapshotWriter(t *testing.T, reader *Store) *Store {
	t.Helper()
	writer, err := OpenWithClock(context.Background(), storePath(reader), reader.now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { writer.Close() })
	return writer
}

func TestStateSnapshotAcrossTransitionStart(t *testing.T) {
	ctx := context.Background()
	s, _, _ := armedFixture(t)
	writer := snapshotWriter(t, s)
	before := mustState(t, s)
	tr := idleDrain(t, writer, "snapshot-start")
	q := &snapshotQuerier{db: s.db, commit: func(ctx context.Context) error {
		_, err := writer.StartTransition(ctx, before.Version, tr)
		return err
	}}
	got, err := readState(ctx, q)
	if q.err != nil {
		t.Fatalf("concurrent transition: %v", q.err)
	}
	if err != nil || !reflect.DeepEqual(got, before) {
		t.Fatalf("state mixed transition snapshots: got=%+v before=%+v err=%v", got, before, err)
	}
	after := mustState(t, s)
	if after.Version <= before.Version || after.Phase != control.PhaseDraining || after.PendingIdleDeadline != nil {
		t.Fatalf("transition did not commit and disarm: %+v", after)
	}
}

func TestStateSnapshotAcrossTransitionFinishAndArm(t *testing.T) {
	ctx := context.Background()
	s, _ := policyFixture(t)
	revision := enablePolicy(t, s)
	current := mustState(t, s)
	tr := idleDrain(t, s, "snapshot-finish")
	target := current
	target.DesiredWorkload = control.WorkloadMedia
	target.ActiveWorkload = control.WorkloadMedia
	tr.Target = target
	started, err := s.StartTransition(ctx, current.Version, tr)
	if err != nil {
		t.Fatal(err)
	}
	target.LeaseFence = started.LeaseFence
	before := mustState(t, s)
	writer := snapshotWriter(t, s)
	deadline := policyEpoch.Add(5 * time.Minute)
	q := &snapshotQuerier{db: s.db, commit: func(ctx context.Context) error {
		if _, err := writer.FinishTransition(ctx, tr.ID, "committed", started.Version, target); err != nil {
			return err
		}
		return writer.ArmIdleDeadline(ctx, revision, deadline, policyEpoch)
	}}
	got, err := readState(ctx, q)
	if q.err != nil {
		t.Fatalf("concurrent finish and arm: %v", q.err)
	}
	if err != nil || !reflect.DeepEqual(got, before) {
		t.Fatalf("valid draining snapshot was mixed with armed state: got=%+v before=%+v err=%v", got, before, err)
	}
	after := mustState(t, s)
	if after.Version <= before.Version || after.Phase != control.PhaseStable || after.PendingIdleDeadline == nil || !after.PendingIdleDeadline.Equal(deadline) {
		t.Fatalf("finish and arm did not commit: %+v", after)
	}
}

func TestSettingsSnapshotAcrossPolicyChanges(t *testing.T) {
	for _, change := range []struct {
		name     string
		from, to int
	}{
		{"enable and arm", 0, 5},
		{"change timeout", 5, 30},
		{"disable", 5, 0},
	} {
		t.Run(change.name, func(t *testing.T) {
			ctx := context.Background()
			s, _ := policyFixture(t)
			if change.from != 0 {
				revision := enablePolicy(t, s)
				if err := s.ArmIdleDeadline(ctx, revision, policyEpoch.Add(5*time.Minute), policyEpoch); err != nil {
					t.Fatal(err)
				}
			}
			before, err := s.Settings(ctx)
			if err != nil {
				t.Fatal(err)
			}
			e := snapshotSettingsPrecondition(t, s, before.SettingsRevision)
			writer := snapshotWriter(t, s)
			q := &snapshotQuerier{db: s.db, commit: func(ctx context.Context) error {
				changed, err := writer.SetIdlePolicy(ctx, e, control.IdlePolicy{TimeoutMinutes: change.to}, true)
				if err != nil || change.to != 5 {
					return err
				}
				return writer.ArmIdleDeadline(ctx, changed.SettingsRevision, policyEpoch.Add(5*time.Minute), policyEpoch)
			}}
			got, err := readPolicyState(ctx, q)
			if q.err != nil {
				t.Fatalf("concurrent settings change: %v", q.err)
			}
			if err != nil || !reflect.DeepEqual(got, before) {
				t.Fatalf("settings mixed revisions: got=%+v before=%+v err=%v", got, before, err)
			}
			after, err := s.Settings(ctx)
			if err != nil || after.SettingsRevision == before.SettingsRevision || after.Policy.TimeoutMinutes != change.to || (after.ArmedDeadline != nil) != (change.to == 5) {
				t.Fatalf("settings change did not commit: %+v, %v", after, err)
			}
		})
	}
}

func TestStateSnapshotAcrossPolicyChange(t *testing.T) {
	ctx := context.Background()
	s, _, _ := armedFixture(t)
	before := mustState(t, s)
	settings, err := s.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e := snapshotSettingsPrecondition(t, s, settings.SettingsRevision)
	writer := snapshotWriter(t, s)
	q := &snapshotQuerier{db: s.db, commit: func(ctx context.Context) error {
		_, err := writer.SetIdlePolicy(ctx, e, control.IdlePolicy{TimeoutMinutes: 30}, true)
		return err
	}}
	got, err := readState(ctx, q)
	if q.err != nil {
		t.Fatalf("concurrent settings change: %v", q.err)
	}
	if err != nil || !reflect.DeepEqual(got, before) {
		t.Fatalf("state lost its policy snapshot: got=%+v before=%+v err=%v", got, before, err)
	}
	after := mustState(t, s)
	if after.IdlePolicy.TimeoutMinutes != 30 || after.PendingIdleDeadline != nil {
		t.Fatalf("settings change did not commit: %+v", after)
	}
}

func snapshotSettingsPrecondition(t *testing.T, s *Store, revision string) control.SettingsPrecondition {
	t.Helper()
	state := mustState(t, s)
	catalog, err := s.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return control.SettingsPrecondition{
		Incarnation: state.LeaseFence.Incarnation, Version: state.Version, Owner: state.Owner,
		ConfigurationRevision: catalog.Revision, SettingsRevision: revision,
	}
}

func TestStateAndSettingsReadOnlyWhileWriterHeld(t *testing.T) {
	for _, mode := range []string{"read-only file", "query-only connection"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			writer, _, _ := armedFixture(t)
			before := mustState(t, writer)
			settings, err := writer.Settings(ctx)
			if err != nil {
				t.Fatal(err)
			}
			options := url.Values{"_txlock": {"immediate"}, "_pragma": {"busy_timeout(0)"}}
			if mode == "read-only file" {
				options.Set("mode", "ro")
			} else {
				options.Add("_pragma", "query_only(ON)")
			}
			dsn := url.URL{Scheme: "file", Path: storePath(writer), RawQuery: options.Encode()}
			db, err := sql.Open("sqlite", dsn.String())
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			reader := &Store{db: db}
			tx, err := writer.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.ExecContext(ctx, `UPDATE control_state SET version = version + 1;
				UPDATE operator_settings SET idle_timeout_minutes = 30;
				UPDATE idle_policy_state SET armed_deadline = NULL, attestation_at = NULL`); err != nil {
				t.Fatal(err)
			}
			if got, err := reader.State(ctx); err != nil || !reflect.DeepEqual(got, before) {
				t.Fatalf("read-only State during write: %+v, %v", got, err)
			}
			if got, err := reader.Settings(ctx); err != nil || !reflect.DeepEqual(got, settings) {
				t.Fatalf("read-only Settings during write: %+v, %v", got, err)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `UPDATE control_state SET version = version + 1`); err == nil {
				t.Fatal("read-only fixture allowed a write")
			}
		})
	}
}
