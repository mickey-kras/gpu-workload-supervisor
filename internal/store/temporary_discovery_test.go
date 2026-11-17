package store

import (
	"context"
	"encoding/json"
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
	next, err := s.StartTransition(ctx, current.Version, tr)
	if err != nil {
		t.Fatal(err)
	}
	v.Fence = next.LeaseFence
	v.Version = next.Version
	v.ConfigurationRevision = e.ConfigurationRevision
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO temporary_discovery_sessions(id,payload,status) VALUES(?,?,?)`, v.ID, raw, v.Status); err != nil {
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
	if err != nil || done != nil {
		t.Fatalf("%+v %v", done, err)
	}
	after, err = s.State(ctx)
	if err != nil || after.Admission != control.AdmissionClosed || after.Phase != control.PhaseStable || after.ActiveWorkload != control.WorkloadIdle {
		t.Fatalf("%+v %v", after, err)
	}
}

func TestTemporaryCleanupRejectsPayloadStatusMismatch(t *testing.T) {
	s, _, record := startTemporaryFixture(t)
	defer s.Close()
	record.Status = "completed"
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE temporary_discovery_sessions SET payload=? WHERE id=?`, raw, record.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckTemporaryDiscovery(t.Context(), record.ID, record.Token); err == nil {
		t.Fatal("mismatched status accepted")
	}
	if _, err := s.TemporaryDiscoveryStatus(t.Context()); err == nil {
		t.Fatal("mismatched status exposed")
	}
}

func TestTemporaryCleanupInspectionRejectsSchemaDriftWithoutMigration(t *testing.T) {
	for _, mode := range []string{"older", "future"} {
		t.Run(mode, func(t *testing.T) {
			s, _, _ := startTemporaryFixture(t)
			defer s.Close()
			expected := len(migrations) - 1
			if mode == "older" {
				if _, err := s.db.Exec(`DELETE FROM schema_migrations WHERE version=?`, len(migrations)); err != nil {
					t.Fatal(err)
				}
			} else {
				expected = len(migrations) + 1
				if _, err := s.db.Exec(`INSERT INTO schema_migrations(version,applied_at) VALUES(?,?)`, expected, formatTime(s.now())); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := InspectTemporaryDiscoveryStatus(t.Context(), s.db); err == nil {
				t.Fatal("incompatible schema accepted")
			}
			var current int
			if err := s.db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&current); err != nil || current != expected {
				t.Fatal("inspection changed schema", current, err)
			}
		})
	}
}

func TestTemporaryCleanupRejectsMissingAndMalformedRecords(t *testing.T) {
	s, _, record := startTemporaryFixture(t)
	defer s.Close()
	if err := s.CheckTemporaryDiscovery(t.Context(), "missing", record.Token); err == nil {
		t.Fatal("missing session accepted")
	}
	for _, raw := range []string{`{`, `{"id":"temporary-one","token":"private-token","status":"running","priorStopped":true}`} {
		if _, err := s.db.Exec(`UPDATE temporary_discovery_sessions SET payload=? WHERE id=?`, []byte(raw), record.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.TemporaryDiscoveryStatus(t.Context()); err == nil {
			t.Fatal("corrupt session accepted")
		}
		if _, err := s.FinishTemporaryDiscovery(t.Context(), record.ID, record.Token); err == nil {
			t.Fatal("corrupt session finalized")
		}
	}
	s.Close()
	if _, err := s.TemporaryDiscoveryStatus(t.Context()); err == nil {
		t.Fatal("storage failure hidden")
	}
	if _, err := InspectTemporaryDiscoveryStatus(t.Context(), s.db); err == nil {
		t.Fatal("inspection failure hidden")
	}
}
