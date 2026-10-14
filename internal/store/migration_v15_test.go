package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

func TestIdleTimeoutDatabaseBoundaries(t *testing.T) {
	s := testStore(t)
	for _, tc := range []struct {
		name  string
		value any
		valid bool
	}{
		{"negative", -1, false}, {"off", 0, true},
		{"one", 1, false}, {"two", 2, false}, {"three", 3, false}, {"four", 4, false},
		{"minimum", 5, true}, {"maximum", 1440, true}, {"above maximum", 1441, false},
		{"fraction below minimum", 0.5, false}, {"fraction above minimum", 5.5, false},
		{"fraction below maximum", 1439.5, false}, {"fraction above maximum", 1440.5, false},
		{"fraction text", "5.5", false}, {"nonnumeric text", "five", false}, {"blob", []byte("5"), false},
		// INTEGER affinity converts integral numeric values and text to integer
		// storage; those remain safe for the application's int scan.
		{"integral real", 5.0, true}, {"integer text", "5", true},
		{"integral real text", "1440.0", true}, {"numeric exponent text", "5e0", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.db.Exec(`UPDATE operator_settings SET idle_timeout_minutes=? WHERE singleton=1`, tc.value)
			if tc.valid {
				if err != nil {
					t.Fatal(err)
				}
				var stored int
				var storage string
				if err := s.db.QueryRow(`SELECT idle_timeout_minutes,typeof(idle_timeout_minutes) FROM operator_settings`).Scan(&stored, &storage); err != nil || storage != "integer" {
					t.Fatalf("unsafe storage: %d %s %v", stored, storage, err)
				}
				return
			}
			var typed *sqlite.Error
			if !errors.As(err, &typed) || typed.Code() != sqlite3.SQLITE_CONSTRAINT_CHECK {
				t.Fatalf("timeout %v: %v", tc.value, err)
			}
		})
	}
}

func TestV15PopulatedUpgrade(t *testing.T) {
	for _, tc := range []struct {
		timeout any
		invalid bool
	}{
		{0, false}, {1, true}, {2, true}, {3, true}, {4, true}, {5, false}, {1440, false},
		{0.5, true}, {5.5, true}, {1439.5, true}, {"5.5", true},
		{5.0, false}, {"5", false}, {"1440.0", false}, {"5e0", false},
	} {
		timeout := tc.timeout
		t.Run(fmt.Sprintf("%T-%v", timeout, timeout), func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "state.db")
			createV1Database(t, path, false)
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			for index := 1; index < 14; index++ {
				if _, err := db.Exec(migrations[index]); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`INSERT INTO schema_migrations VALUES(?,?)`, index+1, formatTime(fixedClock()())); err != nil {
					t.Fatal(err)
				}
			}
			timestamp := formatTime(fixedClock()())
			if _, err := db.Exec(`INSERT INTO operator_settings VALUES(1,'original-revision',?,?)`, timeout, timestamp); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO idle_policy_state VALUES(1,?,NULL,NULL,?)`, timestamp, timestamp); err != nil {
				t.Fatal(err)
			}
			var original any
			var originalStorage string
			if err := db.QueryRow(`SELECT idle_timeout_minutes,typeof(idle_timeout_minutes) FROM operator_settings`).Scan(&original, &originalStorage); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			s, err := Open(ctx, path)
			invalid := tc.invalid
			if invalid {
				if err == nil {
					s.Close()
					t.Fatal("incompatible timeout accepted")
				}
				if !strings.Contains(err.Error(), "schema v15") || !strings.Contains(err.Error(), "0 (off) or between 5 and 1440") {
					t.Fatalf("unclear upgrade error: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				s.Close()
			}
			db, err = sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var version, work int
			var actual any
			var storage string
			var revision, updated, activity string
			if err := db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
				t.Fatal(err)
			}
			wantVersion := 15
			if invalid {
				wantVersion = 14
			}
			if version != wantVersion {
				t.Fatalf("version %d, want %d", version, wantVersion)
			}
			if err := db.QueryRow(`SELECT revision,idle_timeout_minutes,updated_at,typeof(idle_timeout_minutes) FROM operator_settings`).Scan(&revision, &actual, &updated, &storage); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(actual, original) || storage != originalStorage || revision != "original-revision" || updated != timestamp {
				t.Fatalf("settings changed: %q %v %q (%s)", revision, actual, updated, storage)
			}
			if err := db.QueryRow(`SELECT last_activity_at FROM idle_policy_state`).Scan(&activity); err != nil || activity != timestamp {
				t.Fatalf("activity changed: %q %v", activity, err)
			}
			if err := db.QueryRow(`SELECT COUNT(*) FROM registered_work`).Scan(&work); err != nil || work != 1 {
				t.Fatalf("work changed: %d %v", work, err)
			}
			var staging int
			if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='operator_settings_new'`).Scan(&staging); err != nil || staging != 0 {
				t.Fatalf("staging table retained: %d %v", staging, err)
			}
			if !invalid {
				for _, value := range []any{4, 5.5, "5.5"} {
					if _, err := db.Exec(`UPDATE operator_settings SET idle_timeout_minutes=?`, value); err == nil {
						t.Fatalf("upgraded schema accepted %v", value)
					}
				}
			}
		})
	}
}
