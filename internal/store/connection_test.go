package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestConnectionPragmasAfterInterruptedQuery(t *testing.T) {
	s := testStore(t)
	check := func() {
		t.Helper()
		var foreign, busy int
		if err := s.db.QueryRow("PRAGMA foreign_keys").Scan(&foreign); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRow("PRAGMA busy_timeout").Scan(&busy); err != nil {
			t.Fatal(err)
		}

		var journal string
		var synchronous int
		if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRow("PRAGMA synchronous").Scan(&synchronous); err != nil {
			t.Fatal(err)
		}
		if journal != "wal" || synchronous != 2 {
			t.Errorf("durability settings changed: journal=%s synchronous=%d", journal, synchronous)
		}
		if foreign != 1 || busy != 5000 {
			t.Errorf("connection-local safety configuration lost")
		}
	}
	check()
	interruptConnection(t, s)

	check()
}
func TestConnectionRejectsOrphanAuditAfterTimeout(t *testing.T) {
	s := testStore(t)
	event := TransitionEvent{TransitionID: "never-existed", Phase: "stable", Kind: "intent", Action: "test"}
	if err := s.AppendTransitionEvent(context.Background(), event); err == nil {
		t.Fatal("foreign key initially absent")
	}
	interruptConnection(t, s)
	if err := s.AppendTransitionEvent(context.Background(), event); err == nil {
		t.Fatal("orphan audit accepted after timeout")
	}
}

func TestConnectionContentionAfterTimeout(t *testing.T) {
	s := testStore(t)
	initial, err := s.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.StartTransition(context.Background(), initial.Version, Transition{ID: "running", Target: initial, Previous: initial, Deadline: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	writer, err := sql.Open("sqlite", storePath(s)+"?_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	tryContention := func() {
		t.Helper()
		tx, err := writer.Begin()
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { time.Sleep(100 * time.Millisecond); tx.Rollback(); close(done) }()
		started := time.Now()
		err = s.AppendTransitionEvent(context.Background(), TransitionEvent{TransitionID: "running", Phase: "draining", Kind: "intent", Action: "test"})
		t.Logf("journal write elapsed=%v error=%v", time.Since(started), err)
		<-done
		if err != nil {
			t.Error("legitimate writer contention no longer waits")
		}
	}
	tryContention()
	interruptConnection(t, s)
	tryContention()
}

func interruptConnection(t *testing.T, s *Store) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := s.db.ExecContext(ctx, "WITH RECURSIVE c(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM c WHERE x<1000000000) SELECT sum(x) FROM c")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected interrupted query, got %v", err)
	}
}
