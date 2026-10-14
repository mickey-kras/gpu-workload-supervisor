package store

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestSettingsRejectsCorruptPersistedPolicy(t *testing.T) {
	for _, test := range []struct {
		name  string
		query string
	}{
		{"revision", "UPDATE operator_settings SET revision = ''"},
		{"timeout", "UPDATE operator_settings SET idle_timeout_minutes = 1"},
		{"settings timestamp", "UPDATE operator_settings SET updated_at = 'invalid'"},
		{"activity timestamp", "UPDATE idle_policy_state SET last_activity_at = 'invalid'"},
		{"armed timestamp", "UPDATE idle_policy_state SET armed_deadline = 'invalid'"},
		{"attestation timestamp", "UPDATE idle_policy_state SET attestation_at = 'invalid'"},
		{"policy timestamp", "UPDATE idle_policy_state SET updated_at = 'invalid'"},
		{"missing settings", "DELETE FROM operator_settings"},
		{"missing bookkeeping", "DELETE FROM idle_policy_state"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := settingsStore(t)
			ctx := context.Background()
			e, _ := stableSettingsFixture(t, s)
			// Model persisted corruption, including values rejected by current schema constraints.
			if _, err := s.db.ExecContext(ctx, "PRAGMA ignore_check_constraints = ON"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.ExecContext(ctx, test.query); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Settings(ctx); err == nil {
				t.Fatal("corrupt policy accepted")
			}
			if _, err := s.SetIdlePolicy(ctx, e, control.IdlePolicy{TimeoutMinutes: 60}, true); err == nil {
				t.Fatal("settings mutation accepted corrupt policy")
			}
			if test.name == "missing settings" {
				return
			}
			expectedRevision := e.SettingsRevision
			if test.name == "revision" {
				expectedRevision = ""
			}
			var revision string
			err := s.db.QueryRowContext(ctx, "SELECT revision FROM operator_settings WHERE singleton = 1").Scan(&revision)
			if err != nil || revision != expectedRevision {
				t.Fatalf("rejected settings mutation changed revision: %q, %v", revision, err)
			}
		})
	}
}

func TestSetIdlePolicyRollsBackSettingsWhenDeadlineDisarmFails(t *testing.T) {
	s := settingsStore(t)
	ctx := context.Background()
	e, _ := stableSettingsFixture(t, s)
	before, err := s.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER reject_disarm BEFORE UPDATE ON idle_policy_state
		BEGIN SELECT RAISE(ABORT, 'deadline store unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetIdlePolicy(ctx, e, control.IdlePolicy{TimeoutMinutes: 60}, true); err == nil || !strings.Contains(err.Error(), "deadline store unavailable") {
		t.Fatalf("settings failure = %v", err)
	}
	after, err := s.Settings(ctx)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("failed disarm partially committed settings: %+v, %v", after, err)
	}
	if _, err := s.db.ExecContext(ctx, "DROP TRIGGER reject_disarm"); err != nil {
		t.Fatal(err)
	}
	updated, err := s.SetIdlePolicy(ctx, e, control.IdlePolicy{TimeoutMinutes: 60}, true)
	if err != nil || updated.Policy.TimeoutMinutes != 60 || updated.SettingsRevision == e.SettingsRevision {
		t.Fatalf("retry failed to commit: %+v, %v", updated, err)
	}
}
