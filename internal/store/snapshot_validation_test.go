package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

func TestStateAndSettingsRejectMissingSingletons(t *testing.T) {
	for _, table := range []string{"control_state", "operator_settings", "idle_policy_state"} {
		t.Run(table, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			if _, err := s.db.ExecContext(ctx, "DELETE FROM "+table); err != nil {
				t.Fatal(err)
			}
			if _, err := s.State(ctx); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("State with missing %s: %v", table, err)
			}
			_, err := s.Settings(ctx)
			if table == "control_state" {
				if err != nil {
					t.Fatalf("Settings unexpectedly depends on control state: %v", err)
				}
			} else if !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("Settings with missing %s: %v", table, err)
			}
		})
	}
}

func TestStateAndSettingsValidateAllPolicyFields(t *testing.T) {
	for _, corrupt := range []struct {
		name, query, want string
	}{
		{"revision", `UPDATE operator_settings SET revision = ''`, "operator settings revision is empty"},
		// Inject corrupt storage deliberately; normal writes must obey schema v15.
		{"timeout", `PRAGMA ignore_check_constraints=ON; UPDATE operator_settings SET idle_timeout_minutes = 1; PRAGMA ignore_check_constraints=OFF`, "idle timeout must be"},
		{"settings timestamp", `UPDATE operator_settings SET updated_at = 'bad'`, "parsing time"},
		{"activity", `UPDATE idle_policy_state SET last_activity_at = 'bad'`, "parsing time"},
		{"deadline", `UPDATE idle_policy_state SET armed_deadline = 'bad'`, "parsing time"},
		{"attestation", `UPDATE idle_policy_state SET attestation_at = 'bad'`, "parsing time"},
		{"policy timestamp", `UPDATE idle_policy_state SET updated_at = 'bad'`, "parsing time"},
	} {
		t.Run(corrupt.name, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			if _, err := s.db.ExecContext(ctx, corrupt.query); err != nil {
				t.Fatal(err)
			}
			if _, err := s.State(ctx); err == nil || !strings.Contains(err.Error(), corrupt.want) {
				t.Errorf("State with corrupt %s: %v, want %q", corrupt.name, err, corrupt.want)
			}
			if _, err := s.Settings(ctx); err == nil || !strings.Contains(err.Error(), corrupt.want) {
				t.Errorf("Settings with corrupt %s: %v, want %q", corrupt.name, err, corrupt.want)
			}
		})
	}
}

func TestStateSnapshotValidatesControlAndPendingDeadline(t *testing.T) {
	for _, corrupt := range []struct {
		name, query, want string
	}{
		{"timestamp", `UPDATE control_state SET updated_at = 'bad'`, "parsing time"},
		{"control fields", `UPDATE control_state SET lease_incarnation = ''`, "fence incarnation is empty"},
		{"closed with deadline", `UPDATE idle_policy_state SET armed_deadline = '2026-10-01T12:05:00Z'`, "pending idle deadline requires"},
	} {
		t.Run(corrupt.name, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			if _, err := s.db.ExecContext(ctx, corrupt.query); err != nil {
				t.Fatal(err)
			}
			if _, err := s.State(ctx); err == nil || !strings.Contains(err.Error(), corrupt.want) {
				t.Fatalf("State with corrupt %s: %v, want %q", corrupt.name, err, corrupt.want)
			}
		})
	}
}
