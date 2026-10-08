package setup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/operator"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

// Keep a live WAL writer open so discovery must read the committed WAL contents
// without checkpointing, seeding, repairing, or migrating the older deployment.
func temporaryV12Fixture(t *testing.T) (Backend, string, Request, Profile, *sql.DB) {
	t.Helper()
	b, home, request := fixture(t)
	ctx := context.Background()
	if err := b.Apply(ctx, home, request); err != nil {
		t.Fatal(err)
	}
	raw, err := privateRead(filepath.Join(home, ".config/gpu-workload-supervisor/operator.json"))
	if err != nil {
		t.Fatal(err)
	}
	var p Profile
	if err = json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	b.temporaryProfile = func() (Profile, error) { return p, nil }
	db, err := sql.Open("sqlite", p.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err = db.ExecContext(ctx, `DROP TABLE temporary_discovery_sessions; DELETE FROM schema_migrations WHERE version=13; UPDATE control_state SET version=version+1`); err != nil {
		t.Fatal(err)
	}
	// schemaV13 creates only the session table and its partial unique index.
	// Dropping the table also drops that index, exactly restoring v12's schema.
	var sessionObjects int
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name IN ('temporary_discovery_sessions','idx_temporary_discovery_active')`).Scan(&sessionObjects); err != nil || sessionObjects != 0 {
		t.Fatalf("v13 session schema survived fixture downgrade: %d %v", sessionObjects, err)
	}
	request.Profile = p
	snapshot, err := ReadCatalog(ctx, p.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	request.Catalog = snapshot.Catalog
	request.ExpectedRevision = snapshot.Revision
	return b, home, request, p, db
}
func temporaryDatabaseBytes(t *testing.T, home, path string) map[string][]byte {
	t.Helper()
	result := map[string][]byte{}
	for _, name := range []string{path, path + "-wal", path + ".deployment.json", filepath.Join(home, ".config/gpu-workload-supervisor/operator.json")} {
		raw, err := os.ReadFile(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		result[name] = raw
	}
	return result
}
func TestTemporaryV12StatusAndLifecycleVerbsDoNotMigrateLiveState(t *testing.T) {
	b, home, _, p, db := temporaryV12Fixture(t)
	ctx := context.Background()
	b.makeRuntime = func(Request) (gpuruntime.Manager, error) {
		t.Fatal("read-only or old-schema temporary action constructed runtime")
		return nil, nil
	}
	b.runCommand = func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("old-schema temporary action observed runtime")
		return nil, nil
	}
	catalog, err := ReadCatalog(ctx, p.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	before := temporaryDatabaseBytes(t, home, p.StatePath)
	status, err := b.TemporaryStatus(ctx, home)
	if err != nil || status.Available || status.Expected != nil || status.Session != nil || !strings.Contains(status.Reason, "backup") {
		t.Fatalf("old schema advertised actionable status: %+v %v", status, err)
	}
	var incarnation, owner string
	var version uint64
	if err = db.QueryRowContext(ctx, `SELECT lease_incarnation,owner,version FROM control_state WHERE singleton=1`).Scan(&incarnation, &owner, &version); err != nil {
		t.Fatal(err)
	}
	request := TemporaryDiscoveryRequest{Unit: "ollama.service", Expected: operator.Expected{Incarnation: incarnation, Version: strconv.FormatUint(version, 10), Owner: control.Owner(owner), ConfigurationRevision: catalog.Revision}, Consent: true, ExternalControlPaused: true}
	if _, err = b.TemporaryDiscover(ctx, home, request); !errors.Is(err, store.ErrTemporaryDiscoveryUpgradeRequired) {
		t.Fatalf("old-schema start not refused: %v", err)
	}
	if _, err = b.TemporaryCleanup(ctx, home, TemporaryCleanupRequest{ID: "prior-session", Token: "prior-token", ExternalControlPaused: true}); !errors.Is(err, store.ErrTemporaryDiscoveryUpgradeRequired) {
		t.Fatalf("old-schema cleanup not refused: %v", err)
	}
	after := temporaryDatabaseBytes(t, home, p.StatePath)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("temporary action changed live database, WAL, profile, or deployment marker")
	}
	again, err := ReadCatalog(ctx, p.StatePath)
	if err != nil || !reflect.DeepEqual(catalog, again) {
		t.Fatalf("accepted old catalog changed: %+v %v", again, err)
	}
	var schema, sessionTable int
	if err = db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&schema); err != nil || schema != 12 {
		t.Fatalf("old binary schema fence changed: %d %v", schema, err)
	}
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name='temporary_discovery_sessions'`).Scan(&sessionTable); err != nil || sessionTable != 0 {
		t.Fatalf("discovery created session schema: %d %v", sessionTable, err)
	}
}
func TestTemporarySchemaUpgradeStillRequiresBackedUpNormalActivation(t *testing.T) {
	b, home, request, p, db := temporaryV12Fixture(t)
	ctx := context.Background()
	before, err := ReadCatalog(ctx, p.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if err = b.Apply(ctx, home, request); err != nil {
		t.Fatal(err)
	}
	backups, err := filepath.Glob(filepath.Join(home, ".config/gpu-workload-supervisor/backups/activation-*/state.db"))
	if err != nil || len(backups) == 0 {
		t.Fatalf("compatible backup missing: %v %v", backups, err)
	}
	found := false
	for _, path := range backups {
		snapshot, err := readOnlyDB(path)
		if err != nil {
			t.Fatal(err)
		}
		var version int
		err = snapshot.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&version)
		snapshot.Close()
		if err != nil {
			t.Fatal(err)
		}
		if version == 12 {
			saved, err := ReadCatalog(ctx, path)
			if err != nil || !reflect.DeepEqual(saved, before) {
				t.Fatalf("old compatible backup changed: %+v %v", saved, err)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("normal activation migrated before taking the v12 backup")
	}
	var schema int
	if err = db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&schema); err != nil || schema != 13 {
		t.Fatalf("normal activation did not migrate: %d %v", schema, err)
	}
	status, err := b.TemporaryStatus(ctx, home)
	if err != nil || !status.Available || status.Expected == nil {
		t.Fatalf("current schema unavailable after normal activation: %+v %v", status, err)
	}
}
func TestTemporaryCurrentStatusUsesReadOnlyEligibilityAndPendingSession(t *testing.T) {
	b, home, p, r := activatedTemporaryFixture(t)
	ctx := context.Background()
	stateStore, err := store.Open(ctx, p.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()
	before := temporaryDatabaseBytes(t, home, p.StatePath)
	b.makeRuntime = func(Request) (gpuruntime.Manager, error) { t.Fatal("status constructed runtime"); return nil, nil }
	status, err := b.TemporaryStatus(ctx, home)
	if err != nil || !status.Available || status.Expected == nil || status.Session != nil {
		t.Fatalf("current status %+v %v", status, err)
	}
	if after := temporaryDatabaseBytes(t, home, p.StatePath); !reflect.DeepEqual(before, after) {
		t.Fatal("current status changed database or WAL")
	}
	b.makeRuntime = func(Request) (gpuruntime.Manager, error) { return r, nil }
	r.stopErr = errors.New("descendant remains")
	result, err := b.TemporaryDiscover(ctx, home, TemporaryDiscoveryRequest{Unit: "ollama.service", Expected: *status.Expected, Consent: true, ExternalControlPaused: true})
	if err != nil || result.Session == nil || result.Error == "" {
		t.Fatalf("failed session %+v %v", result, err)
	}
	before = temporaryDatabaseBytes(t, home, p.StatePath)
	b.makeRuntime = func(Request) (gpuruntime.Manager, error) {
		t.Fatal("pending status constructed runtime")
		return nil, nil
	}
	status, err = b.TemporaryStatus(ctx, home)
	if err != nil || status.Available || status.Expected == nil || status.Session == nil || status.Session.Status != "cleanup_required" || status.Session.ID != result.Session.ID || status.Reason == "" {
		t.Fatalf("pending status %+v %v", status, err)
	}
	if after := temporaryDatabaseBytes(t, home, p.StatePath); !reflect.DeepEqual(before, after) {
		t.Fatal("pending status changed session, database, or WAL")
	}
}
func TestTemporaryReadOnlyStatusPreservesCorruptStateErrors(t *testing.T) {
	for _, schema := range []int{12, 13} {
		t.Run(fmt.Sprint(schema), func(t *testing.T) {
			b, home, _, p, db := temporaryV12Fixture(t)
			ctx := context.Background()
			if schema == 13 {
				if _, err := db.ExecContext(ctx, `CREATE TABLE temporary_discovery_sessions(id TEXT PRIMARY KEY,payload BLOB,status TEXT); INSERT INTO schema_migrations(version,applied_at) VALUES(13,'2026-10-08T00:00:00Z')`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.ExecContext(ctx, `UPDATE control_state SET updated_at='corrupt'`); err != nil {
				t.Fatal(err)
			}
			before := temporaryDatabaseBytes(t, home, p.StatePath)
			if status, err := b.TemporaryStatus(ctx, home); err == nil || status.Available || strings.Contains(status.Reason, "backup") {
				t.Fatalf("corruption hidden as upgrade prerequisite: %+v %v", status, err)
			}
			after := temporaryDatabaseBytes(t, home, p.StatePath)
			for name, raw := range before {
				if !bytes.Equal(raw, after[name]) {
					t.Fatal("status repaired corrupt database")
				}
			}
		})
	}
}
