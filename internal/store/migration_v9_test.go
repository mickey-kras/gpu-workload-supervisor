package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestMigratesPopulatedV9PreservingRowsAndAddingEventIndex(t *testing.T) {
	fixture, err := os.ReadFile("testdata/v9-populated.sql")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(string(fixture)); err != nil {
		t.Fatal(err)
	}
	before := migrationRows(t, db)
	var version, indexCount int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != 9 {
		t.Fatalf("fixture version %d: %v", version, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='idx_transition_events_transition_sequence'`).Scan(&indexCount); err != nil || indexCount != 0 {
		t.Fatalf("fixture already has event index: %d %v", indexCount, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		s, err := open(context.Background(), path, fixedClock(), fixedUUID("must-not-replace-fixture"))
		if err != nil {
			t.Fatal(err)
		}
		after := migrationRows(t, s.db)
		if !reflect.DeepEqual(before, after) {
			t.Errorf("migration/reopen %d changed persisted rows:\nbefore=%v\nafter=%v", attempt, before, after)
		}
		if err := s.db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != len(migrations) {
			t.Errorf("migrated version %d: %v", version, err)
		}
		var a, b, c int
		var plan string
		if err := s.db.QueryRow(`EXPLAIN QUERY PLAN SELECT sequence FROM transition_events WHERE transition_id=? ORDER BY sequence`, "pending").Scan(&a, &b, &c, &plan); err != nil {
			t.Error(err)
		}
		if !strings.Contains(plan, "idx_transition_events_transition_sequence") {
			t.Errorf("migrated event query plan: %s", plan)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// Compare every field, including tokens, NULL completion state, audit sequence
// numbers and relationship rows; counts alone cannot prove preservation.
func migrationRows(t *testing.T, db *sql.DB) map[string][]string {
	t.Helper()
	result := make(map[string][]string)
	for _, table := range []string{"control_state", "transitions", "transition_events", "registered_work", "transition_work", "state_restorations", "work_resolutions"} {
		rows, err := db.Query(`SELECT * FROM ` + table + ` ORDER BY 1,2`)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			result[table] = append(result[table], fmt.Sprintf("%#v", values))
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	return result
}
