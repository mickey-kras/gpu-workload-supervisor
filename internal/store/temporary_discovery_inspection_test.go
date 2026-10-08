package store

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestTemporaryDiscoveryInspectionReadsSameEligibilityWithoutMutation(t *testing.T) {
	s, e := activationFixture(t)
	defer s.Close()
	ctx := context.Background()
	before, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	view, err := InspectTemporaryDiscovery(ctx, s.db)
	if err != nil || view.Eligibility != nil || view.Session != nil || view.Catalog.Revision != e.ConfigurationRevision || view.State.Version != before.Version {
		t.Fatalf("%+v %v", view, err)
	}
	after, err := s.State(ctx)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("inspection mutated durable control state")
	}
	state := before
	state.Owner = control.OwnerUser
	if _, err = s.UpdateState(ctx, before.Version, state); err != nil {
		t.Fatal(err)
	}
	view, err = InspectTemporaryDiscovery(ctx, s.db)
	if err != nil || !errors.Is(view.Eligibility, ErrWrongOwner) {
		t.Fatalf("user ownership became eligible: %+v %v", view, err)
	}
}
func TestTemporaryDiscoveryInspectionReportsPendingSessionWithoutRecovery(t *testing.T) {
	s, before, record := startTemporaryFixture(t)
	defer s.Close()
	ctx := context.Background()
	view, err := InspectTemporaryDiscovery(ctx, s.db)
	if err != nil || view.Session == nil || view.Session.ID != record.ID || view.State.LeaseFence != record.Fence || !errors.Is(view.Eligibility, ErrTransitionRunning) {
		t.Fatalf("pending session hidden: %+v %v", view, err)
	}
	after, err := s.State(ctx)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("inspection recovered or changed pending session")
	}
}
func TestTemporaryDiscoveryInspectionRefusesUpgradeAndCorruption(t *testing.T) {
	for _, mode := range []string{"v12", "v11", "future", "missing-version", "bad-state", "bad-settings", "bad-catalog", "missing-catalog", "bad-session"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := activationFixture(t)
			defer s.Close()
			ctx := context.Background()
			exec := func(query string) {
				t.Helper()
				if _, err := s.db.Exec(query); err != nil {
					t.Fatal(err)
				}
			}
			switch mode {
			case "v12":
				exec(`DROP TABLE temporary_discovery_sessions; DELETE FROM schema_migrations WHERE version=13`)
			case "v11":
				exec(`DROP TABLE temporary_discovery_sessions; DROP TABLE operator_settings; DROP TABLE idle_policy_state; DELETE FROM schema_migrations WHERE version>=12`)
			case "future":
				exec(`INSERT INTO schema_migrations(version,applied_at) VALUES(999,'2026-10-08T00:00:00Z')`)
			case "missing-version":
				exec(`DROP TABLE schema_migrations`)
			case "bad-state":
				exec(`UPDATE control_state SET updated_at='invalid'`)
			case "bad-settings":
				exec(`UPDATE operator_settings SET updated_at='invalid'`)
			case "bad-catalog":
				exec(`UPDATE workload_catalog SET catalog='invalid'`)
			case "missing-catalog":
				exec(`DELETE FROM workload_catalog`)
			case "bad-session":
				state, _ := s.State(ctx)
				snap, _ := s.Catalog(ctx)
				_, err := s.StartTemporaryDiscovery(ctx, control.OperatorPrecondition{Incarnation: state.LeaseFence.Incarnation, Version: state.Version, Owner: state.Owner, ConfigurationRevision: snap.Revision}, Transition{ID: "record", Target: state, ConfigurationRevision: snap.Revision}, control.TemporaryDiscoverySession{ID: "record", Token: "token", PriorStopped: true, Status: "starting"})
				if err != nil {
					t.Fatal(err)
				}
				exec(`UPDATE temporary_discovery_sessions SET payload='invalid'`)
			}
			_, err := InspectTemporaryDiscovery(ctx, s.db)
			if mode == "v12" || mode == "v11" {
				if !errors.Is(err, ErrTemporaryDiscoveryUpgradeRequired) {
					t.Fatalf("old schema did not require activation: %v", err)
				}
				return
			}
			if err == nil || errors.Is(err, ErrTemporaryDiscoveryUpgradeRequired) {
				t.Fatalf("corruption hidden as unavailable upgrade: %v", err)
			}
		})
	}
}
