package setup

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

func TestAcceptanceInspectionRefusesUnfinishedWorkAndTransitions(t *testing.T) {
	for _, kind := range []string{"work", "transition", "foreign-key"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "state.db")
			s, err := store.Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			state, err := s.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			state.ActiveWorkload = control.WorkloadIdle
			state.Phase = control.PhaseStable
			if _, err = s.UpdateState(ctx, state.Version, state); err != nil {
				t.Fatal(err)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			queries := map[string]string{
				"work":        `INSERT INTO registered_work(request_id,lease_incarnation,lease_epoch,registered_at) VALUES('request','incarnation',1,'now')`,
				"transition":  `INSERT INTO transitions(transition_id,lease_incarnation,lease_epoch,source_state,target_state,previous_state,initiator,phase,deadline,status,created_at,updated_at) VALUES('transition','incarnation',1,'{}','{}','{}','test','draining','later','in_progress','now','now')`,
				"foreign-key": `INSERT INTO transition_work(transition_id,request_id) VALUES('missing','missing')`,
			}
			if _, err := db.Exec(queries[kind]); err != nil {
				t.Fatal(err)
			}
			if err := Inspect(ctx, path); err == nil {
				t.Fatal("unsafe snapshot accepted")
			}
		})
	}
}

func TestAcceptanceBackupIncludesWALAndNeverReplacesExistingSnapshot(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	source := filepath.Join(root, "source.db")
	dest := filepath.Join(root, "snapshot.db")
	db, err := sql.Open("sqlite", source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; CREATE TABLE durable(value TEXT); INSERT INTO durable VALUES('before')`); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(source, 0600); err != nil {
		t.Fatal(err)
	}
	if err := Backup(ctx, source, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE durable SET value='after'`); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if err := Backup(ctx, source, dest); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("existing rollback snapshot overwritten")
	}
	snapshot, err := readOnlyDB(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	var value string
	if err := snapshot.QueryRow(`SELECT value FROM durable`).Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != "before" {
		t.Fatalf("backup lost WAL data: %q", value)
	}
}

func TestAcceptanceReadCatalogDoesNotMigrateLegacyOrBrokenState(t *testing.T) {
	for _, kind := range []string{"missing", "legacy", "corrupt", "wrong-columns", "invalid-catalog"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			if kind == "missing" {
				if _, err := ReadCatalog(context.Background(), path); err == nil {
					t.Fatal("missing accepted")
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("readonly query created state")
				}
				return
			}
			if kind == "corrupt" {
				if err := os.WriteFile(path, []byte("not sqlite"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				db, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				query := `CREATE TABLE legacy(value TEXT)`
				if kind == "wrong-columns" {
					query = `CREATE TABLE workload_catalog(singleton INTEGER, unexpected TEXT)`
				}
				if kind == "invalid-catalog" {
					query = `CREATE TABLE workload_catalog(singleton INTEGER,revision TEXT,catalog BLOB); INSERT INTO workload_catalog VALUES(1,'revision','{}')`
				}
				if _, err := db.Exec(query); err != nil {
					t.Fatal(err)
				}
				db.Close()
				if err := os.Chmod(path, 0600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := ReadCatalog(context.Background(), path)
			if kind == "legacy" {
				if err != nil || snapshot.Revision != "" {
					t.Fatalf("legacy inspection: %+v %v", snapshot, err)
				}
			} else if err == nil {
				t.Fatal("broken catalog accepted")
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatal("readonly catalog inspection modified database")
			}
		})
	}
}
