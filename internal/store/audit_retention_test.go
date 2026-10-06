package store

import (
	"context"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"slices"
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
	assertAuditTransitionIDs(t, s, "transitions", []string{"current", "live", "pending"})
	assertAuditTransitionIDs(t, s, "transition_events", []string{"current", "live", "pending"})
	assertAuditTransitionIDs(t, s, "transition_work", []string{"pending"})
	// Exhaust the eligible history: a one-row batch alone can hide a missing pin.
	if count, err := s.PruneAuditHistory(ctx, s.now().Add(-time.Hour), 1024); err != nil || count != 0 {
		t.Fatalf("protected history pruned: %d, %v", count, err)
	}
	assertAuditTransitionIDs(t, s, "transitions", []string{"current", "live", "pending"})
	assertAuditTransitionIDs(t, s, "transition_events", []string{"current", "live", "pending"})
	assertAuditTransitionIDs(t, s, "transition_work", []string{"pending"})
	var completedAt *string
	if err := s.db.QueryRow(`SELECT completed_at FROM registered_work WHERE request_id='unfinished'`).Scan(&completedAt); err != nil || completedAt != nil {
		t.Fatalf("unfinished work changed: %v, %v", completedAt, err)
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

func assertAuditTransitionIDs(t *testing.T, s *Store, table string, want []string) {
	t.Helper()
	rows, err := s.db.Query(`SELECT transition_id FROM ` + table + ` ORDER BY transition_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		got = append(got, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("%s retained IDs %v, want %v", table, got, want)
	}
}

func TestPruneAuditHistoryRejectsInvalidCutoffAndLimit(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, cutoff := range []time.Time{{}, s.now(), s.now().Add(time.Hour)} {
		if _, err := s.PruneAuditHistory(ctx, cutoff, 1); err == nil {
			t.Fatalf("cutoff %v accepted", cutoff)
		}
	}
	for _, limit := range []int{0, -1, 1025} {
		if _, err := s.PruneAuditHistory(ctx, s.now().Add(-time.Hour), limit); err == nil {
			t.Fatalf("limit %d accepted", limit)
		}
	}
}

func TestPruneAuditHistoryRollsBackWhenStorageFails(t *testing.T) {
	for _, mode := range []string{"missing journal", "blocked event delete", "missing recovery audit"} {
		t.Run(mode, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			state, err := s.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			tr := Transition{ID: "old", Fence: state.LeaseFence, Source: state, Target: state, Previous: state, Phase: control.PhaseDraining, Deadline: time.Now()}
			if err := s.BeginTransition(ctx, tr); err != nil {
				t.Fatal(err)
			}
			if err := s.AppendTransitionEvent(ctx, TransitionEvent{TransitionID: tr.ID, Phase: control.PhaseDraining, Kind: "intent", Action: "test"}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.ExecContext(ctx, `UPDATE transitions SET status='failed', updated_at='2000-01-01T00:00:00Z', lease_epoch=99 WHERE transition_id=?`, tr.ID); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "missing journal":
				if _, err := s.db.ExecContext(ctx, "DROP TABLE transition_events; DROP TABLE transitions"); err != nil {
					t.Fatal(err)
				}
			case "blocked event delete":
				if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER reject_event_delete BEFORE DELETE ON transition_events
					BEGIN SELECT RAISE(ABORT, 'event store unavailable'); END`); err != nil {
					t.Fatal(err)
				}
			case "missing recovery audit":
				if _, err := s.db.ExecContext(ctx, "DROP TABLE state_restorations"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.PruneAuditHistory(ctx, s.now().Add(-time.Hour), 10); err == nil {
				t.Fatal("prune committed despite storage failure")
			}
			if mode != "missing journal" {
				assertAuditTransitionIDs(t, s, "transitions", []string{tr.ID})
				assertAuditTransitionIDs(t, s, "transition_events", []string{tr.ID})
			}
		})
	}
}
