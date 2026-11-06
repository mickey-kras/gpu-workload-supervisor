package store

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

// A v18 database upgrades to v19 and keeps its rows. The new limit_stops
// table holds at most one row with a valid workload.
func TestMigratesV18KeepingRowsAndLimitStopConstraints(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	first, err := open(ctx, path, fixedClock(), fixedUUID("11111111-1111-4111-8111-111111111111"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.ReplaceCatalog(ctx, "", control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "text", Label: "Text", Adapter: "systemd", Unit: "text.service", Cgroup: "/user/text", HealthURL: "http://localhost:9000"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := first.db.ExecContext(ctx, `INSERT INTO model_checks(model,payload) VALUES('model-a','{}')`); err != nil {
		t.Fatal(err)
	}
	before := v19Rows(t, first)
	if _, err := first.db.ExecContext(ctx, `DROP TABLE limit_stops; DELETE FROM schema_migrations WHERE version >= 19`); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := open(ctx, path, fixedClock(), fixedUUID("22222222-2222-4222-8222-222222222222"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if after := v19Rows(t, s); !reflect.DeepEqual(before, after) {
		t.Fatalf("upgrade changed persisted rows:\nbefore=%v\nafter=%v", before, after)
	}
	var version int
	if err := s.db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != len(migrations) {
		t.Fatalf("upgraded version %d: %v", version, err)
	}
	var applied int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = 19`).Scan(&applied); err != nil || applied != 1 {
		t.Fatalf("v19 applied %d times: %v", applied, err)
	}
	// The times are Unix nanoseconds, as time.Time.UnixNano gives them.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return strconv.FormatInt(base.Add(d).UnixNano(), 10) }
	valid := `INSERT INTO limit_stops(singleton,workload,first_seen_at) VALUES(1,'text',` + at(0) + `)`
	if _, err := s.db.ExecContext(ctx, valid); err != nil {
		t.Fatalf("valid limit stop: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, valid); err == nil {
		t.Error("a second limit stop was accepted")
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE limit_stops SET workload='idle'`); err == nil {
		t.Error("a limit stop of idle was accepted as an update")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM limit_stops`); err != nil {
		t.Fatal(err)
	}
	// Each refused row differs from a valid row in one column only.
	for name, row := range map[string]string{
		"singleton 2":                `2,'text',` + at(0) + `,NULL,NULL`,
		"empty workload":             `1,'',` + at(0) + `,NULL,NULL`,
		"long workload":              `1,'` + strings.Repeat("w", 65) + `',` + at(0) + `,NULL,NULL`,
		"idle":                       `1,'idle',` + at(0) + `,NULL,NULL`,
		"unknown":                    `1,'unknown',` + at(0) + `,NULL,NULL`,
		"no workload":                `1,NULL,` + at(0) + `,NULL,NULL`,
		"no first seen":              `1,'text',NULL,NULL,NULL`,
		"zero first seen":            `1,'text',0,NULL,NULL`,
		"negative first seen":        `1,'text',-1,NULL,NULL`,
		"text time":                  `1,'text','2026-01-01T00:00:00Z',NULL,NULL`,
		"text definite time":         `1,'text',` + at(0) + `,'2026-01-01T00:00:01Z',NULL`,
		"text stopping time":         `1,'text',` + at(0) + `,` + at(time.Second) + `,'2026-01-01T00:00:02Z'`,
		"stopping without definite":  `1,'text',` + at(0) + `,NULL,` + at(time.Second),
		"definite before first seen": `1,'text',` + at(time.Nanosecond) + `,` + at(0) + `,NULL`,
		"stopping before definite":   `1,'text',` + at(0) + `,` + at(2*time.Second) + `,` + at(time.Second),
	} {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO limit_stops(singleton,workload,first_seen_at,definite_at,stopping_at) VALUES(`+row+`)`); err == nil {
			t.Errorf("%s was accepted", name)
			if _, err := s.db.ExecContext(ctx, `DELETE FROM limit_stops`); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO limit_stops(singleton,workload,first_seen_at) VALUES(1,'`+strings.Repeat("w", 64)+`',`+at(0)+`)`); err != nil {
		t.Fatalf("limit stop with a 64 character workload: %v", err)
	}
	// Ordered times are accepted: 0.5 s apart, 20 ms apart (as RFC 3339
	// text, .1 sorts after .12), and equal neighbours.
	for _, times := range [][3]time.Duration{{0, 500 * time.Millisecond, time.Second}, {100 * time.Millisecond, 120 * time.Millisecond, 120 * time.Millisecond},
		{0, 0, time.Second}} {
		update := `UPDATE limit_stops SET first_seen_at=` + at(times[0]) + `, definite_at=` + at(times[1]) + `, stopping_at=` + at(times[2])
		if _, err := s.db.ExecContext(ctx, update); err != nil {
			t.Fatalf("limit stop with ordered times %v: %v", times, err)
		}
	}
}

// v19Rows reads the rows that the v19 upgrade must keep.
func v19Rows(t *testing.T, s *Store) map[string][]string {
	t.Helper()
	rows := migrationRows(t, s.db)
	for _, query := range []string{`SELECT model, CAST(payload AS TEXT) FROM model_checks ORDER BY model`, `SELECT CAST(revision AS TEXT) FROM workload_catalog`} {
		result, err := s.db.Query(query)
		if err != nil {
			t.Fatal(err)
		}
		columns, _ := result.Columns()
		for result.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := result.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			rows[query] = append(rows[query], fmt.Sprintf("%#v", values))
		}
		if err := result.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return rows
}
