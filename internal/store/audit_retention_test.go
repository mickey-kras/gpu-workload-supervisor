package store

import (
	"context"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"strings"
	"testing"
	"time"
)

func TestAuditRetentionPreservesLiveRelationshipsAndLatestEvidence(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	state, _ := s.State(ctx)
	for _, id := range []string{"old", "live", "pending", "current"} {
		tr := Transition{ID: id, Fence: state.LeaseFence, Source: state, Target: state, Previous: state, Phase: control.PhaseDraining, Deadline: time.Now()}
		if err := s.BeginTransition(ctx, tr); err != nil {
			t.Fatal(err)
		}
		if id == "current" {
			if _, err := s.db.Exec(`UPDATE transitions SET status='failed',updated_at='2000-01-01T00:00:00Z' WHERE transition_id=?`, id); err != nil {
				t.Fatal(err)
			}
		}
		if id != "live" && id != "current" {
			if _, err := s.db.Exec(`UPDATE transitions SET status='failed',updated_at='2000-01-01T00:00:00Z',lease_epoch=99 WHERE transition_id=?`, id); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.AppendTransitionEvent(ctx, TransitionEvent{TransitionID: id, Phase: control.PhaseDraining, Kind: "intent", Action: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO registered_work(request_id,lease_incarnation,lease_epoch,registered_at) VALUES('unfinished','old',1,'2000-01-01T00:00:00Z'); INSERT INTO transition_work VALUES('pending','unfinished')`); err != nil {
		t.Fatal(err)
	}
	count, err := s.PruneAuditHistory(ctx, s.now().Add(-time.Hour), 1)
	if err != nil || count != 1 {
		t.Fatalf("prune %d: %v", count, err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM transitions`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("transitions %d %v", n, err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM transition_events`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("events %d %v", n, err)
	}
}

func TestAuditRetentionKeepsLatestRecoveryRecordsAndUsesEventIndex(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	cutoff := s.now().Add(-time.Hour)
	for i := 0; i < 3; i++ {
		if _, err := s.db.Exec(`INSERT INTO state_restorations(previous_incarnation,previous_epoch,new_incarnation,new_epoch,abandoned_work,invalidated_transitions,created_at) VALUES('old',1,'new',1,0,0,'2000-01-01T00:00:00Z');
 INSERT INTO work_resolutions(lease_incarnation,lease_epoch,state_version,reason,abandoned_work,created_at) VALUES('old',1,1,'test',1,'2000-01-01T00:00:00Z')`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO registered_work(request_id,lease_incarnation,lease_epoch,registered_at) VALUES('pending-audit','old',1,'2000-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PruneAuditHistory(ctx, cutoff, 3); err != nil || n != 0 {
		t.Fatalf("unresolved work lost evidence: %d %v", n, err)
	}
	if _, err := s.db.Exec(`UPDATE registered_work SET completed_at='2001-01-01T00:00:00Z'`); err != nil {
		t.Fatal(err)
	}
	count, err := s.PruneAuditHistory(ctx, cutoff, 3)
	if err != nil || count != 3 {
		t.Fatalf("first batch %d: %v", count, err)
	}
	count, err = s.PruneAuditHistory(ctx, cutoff, 3)
	if err != nil || count != 1 {
		t.Fatalf("second batch %d: %v", count, err)
	}
	for _, table := range []string{"state_restorations", "work_resolutions"} {
		var count, seq int
		if err := s.db.QueryRow(`SELECT COUNT(*),MAX(sequence) FROM `+table).Scan(&count, &seq); err != nil || count != 1 || seq != 3 {
			t.Fatalf("%s retained %d/%d: %v", table, count, seq, err)
		}
	}
	var a, b, c int
	var plan string
	if err := s.db.QueryRow(`EXPLAIN QUERY PLAN SELECT sequence FROM transition_events WHERE transition_id=? ORDER BY sequence`, "history").Scan(&a, &b, &c, &plan); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "idx_transition_events_transition_sequence") {
		t.Fatalf("event plan: %s", plan)
	}
}
