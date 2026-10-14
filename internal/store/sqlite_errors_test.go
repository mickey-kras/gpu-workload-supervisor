package store

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

func TestUniqueConstraintClassification(t *testing.T) {
	s := testStore(t)
	if _, err := s.db.Exec(`CREATE TABLE constraint_parent(id INTEGER PRIMARY KEY);
 CREATE TABLE constraint_fixture(id INTEGER PRIMARY KEY, value TEXT NOT NULL UNIQUE CHECK(length(value)>0), parent INTEGER REFERENCES constraint_parent(id));
 INSERT INTO constraint_fixture VALUES(1,'existing',NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, query string
		code        int
		conflict    bool
	}{
		{"primary key", `INSERT INTO constraint_fixture VALUES(1,'other',NULL)`, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY, true},
		{"unique", `INSERT INTO constraint_fixture VALUES(2,'existing',NULL)`, sqlite3.SQLITE_CONSTRAINT_UNIQUE, true},
		{"check", `INSERT INTO constraint_fixture VALUES(2,'',NULL)`, sqlite3.SQLITE_CONSTRAINT_CHECK, false},
		{"not null", `INSERT INTO constraint_fixture VALUES(2,NULL,NULL)`, sqlite3.SQLITE_CONSTRAINT_NOTNULL, false},
		{"foreign key", `INSERT INTO constraint_fixture VALUES(2,'other',99)`, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.db.Exec(tc.query)
			var typed *sqlite.Error
			if !errors.As(err, &typed) || typed.Code() != tc.code {
				t.Fatalf("got %v; want SQLite code %d", err, tc.code)
			}
			if got := isUniqueConstraint(fmt.Errorf("wrapped: %w", err)); got != tc.conflict {
				t.Fatalf("classification=%v, want %v", got, tc.conflict)
			}
		})
	}
	if isUniqueConstraint(nil) || isUniqueConstraint(errors.New("UNIQUE constraint failed")) {
		t.Fatal("classified untyped error as a conflict")
	}
}

func TestAdmissionsPreserveOtherConstraintErrors(t *testing.T) {
	for _, comfy := range []bool{false, true} {
		t.Run(fmt.Sprintf("comfy=%v", comfy), func(t *testing.T) {
			s := testStore(t)
			a := admittedComfyUIFixture(t, s)
			admit := func() error {
				if comfy {
					_, err := s.AdmitComfyUIWork(context.Background(), a, "")
					return err
				}
				_, err := s.AdmitWorkToken(context.Background(), a.RequestID, "", a.Workload, a.Fence)
				return err
			}
			// Deliberately include the old text classifier's match in an unrelated
			// trigger constraint to verify the actual driver code decides the result.
			if _, err := s.db.Exec(`CREATE TRIGGER reject_registration BEFORE INSERT ON registered_work BEGIN SELECT RAISE(ABORT,'UNIQUE constraint fixture'); END`); err != nil {
				t.Fatal(err)
			}
			err := admit()
			var typed *sqlite.Error
			if errors.Is(err, ErrRequestConflict) || !errors.As(err, &typed) || typed.Code() != sqlite3.SQLITE_CONSTRAINT_TRIGGER {
				t.Fatalf("constraint not preserved: %v", err)
			}
			if count, err := s.PendingWork(context.Background()); err != nil || count != 0 {
				t.Fatalf("failed admission persisted: %d %v", count, err)
			}
			if _, err := s.db.Exec(`DROP TRIGGER reject_registration`); err != nil {
				t.Fatal(err)
			}
			if err := admit(); err != nil {
				t.Fatal(err)
			}
			if err := admit(); !errors.Is(err, ErrRequestConflict) {
				t.Fatalf("duplicate admission: %v", err)
			}
			if count, err := s.PendingWork(context.Background()); err != nil || count != 1 {
				t.Fatalf("duplicate persisted: %d %v", count, err)
			}
		})
	}
}
