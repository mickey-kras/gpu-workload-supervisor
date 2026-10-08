package store

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

// A v11 database is a current database minus the v12 tables; the upgrade must
// recreate them, seed the idle policy Off, and leave every prior row intact.
func TestMigratesV11SeedingSettingsOffAndPreservingRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	first, err := open(ctx, path, fixedClock(), fixedUUID("11111111-1111-4111-8111-111111111111"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.ReplaceCatalog(ctx, "", control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "text", Label: "Text", Adapter: "systemd", Unit: "text.service", Cgroup: "/user/text", HealthURL: "http://localhost:9000"}}}); err != nil {
		t.Fatal(err)
	}
	before := migrationRows(t, first.db)
	if _, err := first.db.ExecContext(ctx, `DROP TABLE temporary_discovery_sessions; DROP TABLE operator_settings;
		DROP TABLE idle_policy_state;
		DELETE FROM schema_migrations WHERE version >= 12`); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := first.db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != 11 {
		t.Fatalf("v11 downgrade failed: version=%d %v", version, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	for attempt := 0; attempt < 2; attempt++ {
		s, err := open(ctx, path, fixedClock(), fixedUUID("22222222-2222-4222-8222-222222222222"))
		if err != nil {
			t.Fatal(err)
		}
		after := migrationRows(t, s.db)
		if !reflect.DeepEqual(before, after) {
			t.Errorf("upgrade %d changed persisted rows:\nbefore=%v\nafter=%v", attempt, before, after)
		}
		if err := s.db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != len(migrations) {
			t.Errorf("upgraded version %d: %v", version, err)
		}
		settings, err := s.Settings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if settings.Policy.TimeoutMinutes != 0 {
			t.Errorf("upgraded policy = %#v, want seeded Off", settings.Policy)
		}
		if settings.SettingsRevision != "22222222-2222-4222-8222-222222222222" {
			t.Errorf("seeded settings revision = %q", settings.SettingsRevision)
		}
		if settings.LastActivityAt == nil || !settings.LastActivityAt.Equal(fixedClock()()) {
			t.Errorf("seeded last activity = %v, want migration time", settings.LastActivityAt)
		}
		if settings.ArmedDeadline != nil || settings.AttestationAt != nil {
			t.Errorf("seeded policy state armed: %#v", settings)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
