package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestRestoreRetiresReservedDemandAndPreservesQueuedDemand(t *testing.T) {
	ctx := context.Background()
	live, _ := policyFixture(t)
	reserved := testDemand()
	if err := live.QueueDemand(ctx, reserved); err != nil {
		t.Fatal(err)
	}
	claim, err := live.ReserveDemand(ctx, reserved)
	if err != nil {
		t.Fatal(err)
	}
	queued := reserved
	queued.RequestID = "queued-demand"
	if err := live.QueueDemand(ctx, queued); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "backup.db")
	if _, err := live.db.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		t.Fatal(err)
	}
	restored, err := OpenRestored(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if _, err := restored.RotateIncarnation(ctx); err != nil {
		t.Fatal(err)
	}
	if err := restored.QueueDemand(ctx, reserved); !errors.Is(err, ErrDemandConflict) {
		t.Fatalf("restored reservation replay: %v", err)
	}
	if err := restored.RequeueDemand(ctx, reserved.RequestID, claim); !errors.Is(err, ErrDemandConflict) {
		t.Fatalf("retired claim restored: %v", err)
	}
	if err := restored.QueueDemand(ctx, queued); err != nil {
		t.Fatalf("queued identity lost: %v", err)
	}
	var status string
	var cleared, abandoned int
	if err := restored.db.QueryRow(`SELECT status,claim IS NULL FROM broker_demands WHERE request_id=?`, reserved.RequestID).Scan(&status, &cleared); err != nil {
		t.Fatal(err)
	}
	if status != "cancelled" || cleared != 1 {
		t.Fatalf("reservation status=%s cleared=%d", status, cleared)
	}
	if err := restored.db.QueryRow(`SELECT abandoned_work FROM state_restorations`).Scan(&abandoned); err != nil || abandoned != 1 {
		t.Fatalf("restore audit count=%d %v", abandoned, err)
	}
	if err := restored.CancelDemand(ctx, queued); err != nil {
		t.Fatal(err)
	}
	var pending int
	if err := restored.withTx(ctx, func(tx *sql.Tx) error { var err error; pending, err = pendingIdleWorkTx(ctx, tx); return err }); err != nil || pending != 0 {
		t.Fatalf("retired reservation blocks idle: %d %v", pending, err)
	}
}

func TestRestoreAuditFailureKeepsReservedDemand(t *testing.T) {
	ctx := context.Background()
	s, _ := policyFixture(t)
	d := testDemand()
	if err := s.QueueDemand(ctx, d); err != nil {
		t.Fatal(err)
	}
	claim, err := s.ReserveDemand(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER reject_demand_restore BEFORE INSERT ON state_restorations BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RotateIncarnation(ctx); err == nil {
		t.Fatal("restore accepted failed audit")
	}
	after, err := s.State(ctx)
	if err != nil || after != before {
		t.Fatalf("failed restore changed state: %v", err)
	}
	if err := s.RequeueDemand(ctx, d.RequestID, claim); err != nil {
		t.Fatalf("failed restore retired reservation: %v", err)
	}
}
