package store

import (
	"context"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestVerifiedResolutionReleasesWorkAndAuditRetentionPins(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := s.now()
	s.now = func() time.Time { return now }
	opened := admitRetentionWork(t, s, "orphan")
	tr := Transition{ID: "orphan-transition", Fence: opened.LeaseFence,
		Source: opened, Target: opened, Previous: opened,
		Phase: control.PhaseDraining, Deadline: now.Add(time.Hour)}
	if err := s.BeginTransition(ctx, tr); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE transitions SET status='failed' WHERE transition_id=?`, tr.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO transition_work(transition_id,request_id) VALUES(?,?)`, tr.ID, "orphan"); err != nil {
		t.Fatal(err)
	}
	rotated, err := s.RotateFenceAndCloseAdmission(ctx, opened.Version)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(48 * time.Hour)
	cutoff := now.Add(-time.Hour)
	if n, err := s.PruneCompletedWork(ctx, cutoff, 100); err != nil || n != 0 {
		t.Fatalf("unresolved work pruned: count=%d error=%v", n, err)
	}
	if n, err := s.PruneAuditHistory(ctx, cutoff, 100); err != nil || n != 0 {
		t.Fatalf("unresolved audit pruned: count=%d error=%v", n, err)
	}
	if !retentionWorkExists(t, s, "orphan") {
		t.Fatal("unresolved registration disappeared")
	}
	if n, err := s.ResolveUnfinishedWork(ctx, rotated.Version, "verified operator recovery"); err != nil || n != 1 {
		t.Fatalf("resolve: count=%d error=%v", n, err)
	}
	// Audit evidence becomes eligible immediately once the work is terminal,
	// without deleting the work row or waiting for another fence rotation.
	if n, err := s.PruneAuditHistory(ctx, cutoff, 100); err != nil || n != 1 {
		t.Fatalf("resolved audit still pinned: count=%d error=%v", n, err)
	}
	var transitions int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM transitions WHERE transition_id=?`, tr.ID).Scan(&transitions); err != nil || transitions != 0 {
		t.Fatalf("resolved transition retained: count=%d error=%v", transitions, err)
	}
	if n, err := s.PruneCompletedWork(ctx, cutoff, 100); err != nil || n != 0 {
		t.Fatalf("recent resolution pruned: count=%d error=%v", n, err)
	}
	now = now.Add(48 * time.Hour)
	if n, err := s.PruneCompletedWork(ctx, now.Add(-time.Hour), 100); err != nil || n != 1 {
		t.Fatalf("resolved work still pinned: count=%d error=%v", n, err)
	}
	if retentionWorkExists(t, s, "orphan") {
		t.Fatal("resolved registration retained past cutoff")
	}
	var reason string
	if err := s.db.QueryRowContext(ctx, `SELECT reason FROM work_resolutions`).Scan(&reason); err != nil || reason != "verified operator recovery" {
		t.Fatalf("latest resolution evidence lost: reason=%q error=%v", reason, err)
	}
}
