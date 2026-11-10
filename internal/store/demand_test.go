package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func testDemand() Demand {
	return Demand{RequestID: "demand-1", Requester: "requester", Workflow: "workflow", BodySHA256: strings.Repeat("a", 64), Workload: control.WorkloadText}
}

func TestDemandIdentityAdmissionAndCompletion(t *testing.T) {
	ctx := context.Background()
	s, _ := policyFixture(t)
	d := testDemand()
	if err := s.QueueDemand(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := s.QueueDemand(ctx, d); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Demand){func(d *Demand) { d.Requester = "other" }, func(d *Demand) { d.Workflow = "other" }, func(d *Demand) { d.BodySHA256 = strings.Repeat("b", 64) }, func(d *Demand) { d.Workload = control.WorkloadMedia }} {
		other := d
		change(&other)
		if err := s.QueueDemand(ctx, other); !errors.Is(err, ErrDemandConflict) {
			t.Fatalf("changed binding accepted: %v", err)
		}
	}
	claim, err := s.ReserveDemand(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CancelDemand(ctx, d); !errors.Is(err, ErrDemandConflict) {
		t.Fatalf("cancel reservation: %v", err)
	}
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdmitWorkToken(ctx, d.RequestID, "", d.Workload, state.LeaseFence); !errors.Is(err, ErrDemandConflict) {
		t.Fatalf("unclaimed admission: %v", err)
	}
	if _, err := s.AdmitWorkToken(WithDemandClaim(ctx, "wrong"), d.RequestID, "", d.Workload, state.LeaseFence); !errors.Is(err, ErrDemandConflict) {
		t.Fatalf("wrong claim: %v", err)
	}
	token, err := s.AdmitWorkToken(WithDemandClaim(ctx, claim), d.RequestID, "", d.Workload, state.LeaseFence)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RequeueDemand(ctx, d.RequestID, claim); !errors.Is(err, ErrDemandConflict) {
		t.Fatalf("admitted requeue: %v", err)
	}
	if err := s.QueueDemand(ctx, d); !errors.Is(err, ErrDemandConflict) {
		t.Fatalf("replay: %v", err)
	}
	if pending, err := s.PendingWork(ctx); err != nil || pending != 1 {
		t.Fatalf("pending=%d %v", pending, err)
	}
	if err := s.FinishWorkToken(ctx, d.RequestID, d.Workload, state.LeaseFence, token, WorkCompleted); err != nil {
		t.Fatal(err)
	}
	if pending, err := s.PendingWork(ctx); err != nil || pending != 0 {
		t.Fatalf("pending=%d %v", pending, err)
	}
}

func TestDemandSurvivesRestartWithoutReservationReplay(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	d := testDemand()
	if err := s.QueueDemand(ctx, d); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveDemand(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.ReserveDemand(ctx, d); !errors.Is(err, ErrDemandConflict) {
		t.Fatalf("restart replay: %v", err)
	}
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveUnfinishedWork(ctx, state.Version, "verified runtimes stopped"); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("unfenced recovery: %v", err)
	}
	state, err = s.RotateFenceAndCloseAdmission(ctx, state.Version)
	if err != nil {
		t.Fatal(err)
	}
	if count, err := s.ResolveUnfinishedWork(ctx, state.Version, "verified runtimes stopped"); err != nil || count != 1 {
		t.Fatalf("recovery=%d %v", count, err)
	}
	if _, err := s.ReserveDemand(ctx, d); !errors.Is(err, ErrDemandConflict) {
		t.Fatalf("recovered replay: %v", err)
	}
}

func TestDemandRequeueCancelRetention(t *testing.T) {
	ctx := context.Background()
	s, now := policyFixture(t)
	d := testDemand()
	if err := s.QueueDemand(ctx, d); err != nil {
		t.Fatal(err)
	}
	claim, err := s.ReserveDemand(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RequeueDemand(ctx, d.RequestID, claim); err != nil {
		t.Fatal(err)
	}
	if err := s.CancelDemand(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := s.QueueDemand(ctx, d); !errors.Is(err, ErrDemandConflict) {
		t.Fatalf("cancelled replay: %v", err)
	}
	*now = now.Add(time.Hour)
	if _, err := s.PruneCompletedWork(ctx, *now, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.QueueDemand(ctx, d); err != nil {
		t.Fatal(err)
	}
}

func TestDemandDoesNotBlockOwnActivation(t *testing.T) {
	s, e := activationFixture(t)
	ctx := context.Background()
	d := testDemand()
	if err := s.QueueDemand(ctx, d); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveDemand(ctx, d); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartActivationTransition(ctx, e, activationTransition(t, s, "activate-demand")); err != nil {
		t.Fatal(err)
	}
}

func TestDemandAdmissionRollbackAndOwnerCheck(t *testing.T) {
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
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER reject_demand_work BEFORE INSERT ON registered_work BEGIN SELECT RAISE(ABORT,'test refusal'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdmitWorkToken(WithDemandClaim(ctx, claim), d.RequestID, "", d.Workload, state.LeaseFence); err == nil {
		t.Fatal("registration unexpectedly succeeded")
	}
	if err := s.RequeueDemand(ctx, d.RequestID, claim); err != nil {
		t.Fatalf("reservation did not roll back: %v", err)
	}
	claim, err = s.ReserveDemand(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER reject_demand_work; UPDATE control_state SET owner='user'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdmitWorkToken(WithDemandClaim(ctx, claim), d.RequestID, "", d.Workload, state.LeaseFence); err == nil {
		t.Fatalf("owner race dispatched: %v", err)
	}
}

func TestDemandCapacityRetainsExistingQueuedIdentity(t *testing.T) {
	ctx := context.Background()
	s, _ := policyFixture(t)
	d := testDemand()
	if err := s.QueueDemand(ctx, d); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<4095)
 INSERT INTO broker_demands(request_id,requester,workflow,body_sha256,workload,status,created_at,updated_at)
 SELECT 'fixture-'||x,'requester','workflow',?,'text','queued',?,? FROM n`, d.BodySHA256, formatTime(policyEpoch), formatTime(policyEpoch)); err != nil {
		t.Fatal(err)
	}
	if err := s.QueueDemand(ctx, d); err != nil {
		t.Fatalf("existing retry at capacity: %v", err)
	}
	other := d
	other.RequestID = "over-limit"
	if err := s.QueueDemand(ctx, other); !errors.Is(err, ErrDemandCapacity) {
		t.Fatalf("capacity ignored: %v", err)
	}
	if _, err := s.PruneCompletedWork(ctx, policyEpoch.Add(time.Hour), 1024); err != nil {
		t.Fatal(err)
	}
	if err := s.QueueDemand(ctx, other); !errors.Is(err, ErrDemandCapacity) {
		t.Fatalf("pruned queued demand: %v", err)
	}
}

func TestDemandMigrationPreservesV15State(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "upgrade.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP TABLE limit_stops; DROP TABLE model_checks; DROP TABLE training_claims; DROP TABLE training_jobs; DROP TABLE broker_demands; DELETE FROM schema_migrations WHERE version>=16`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	after, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before.Version != after.Version || before.LeaseFence != after.LeaseFence {
		t.Fatal("migration changed authority")
	}
	settings, err := s.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Policy.TimeoutMinutes != control.IdlePolicyOff {
		t.Fatal("migration enabled idle")
	}
	if err := s.QueueDemand(ctx, testDemand()); err != nil {
		t.Fatal(err)
	}
}

func TestDemandRetentionDeletesExactCompletedBatch(t *testing.T) {
	ctx := context.Background()
	s, now := policyFixture(t)
	d := testDemand()
	if err := s.QueueDemand(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := s.CancelDemand(ctx, d); err != nil {
		t.Fatal(err)
	}
	d.RequestID = "completed-demand"
	if err := s.QueueDemand(ctx, d); err != nil {
		t.Fatal(err)
	}
	claim, err := s.ReserveDemand(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.AdmitWorkToken(WithDemandClaim(ctx, claim), d.RequestID, "", d.Workload, state.LeaseFence)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishWorkToken(ctx, d.RequestID, d.Workload, state.LeaseFence, token, WorkCompleted); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Hour)
	if n, err := s.PruneCompletedWork(ctx, *now, 1); err != nil || n != 1 {
		t.Fatalf("prune=%d %v", n, err)
	}
	var remaining int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM broker_demands WHERE status='cancelled'`).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatalf("cancelled batch exceeded budget: %d %v", remaining, err)
	}
	if n, err := s.PruneCompletedWork(ctx, *now, 1); err != nil || n != 1 {
		t.Fatalf("cancelled prune=%d %v", n, err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM broker_demands`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("orphaned demand tombstones: %d", count)
	}
}

func TestDemandComfyUIAdmissionIsAtomic(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	attempt := admittedComfyUIFixture(t, s)
	d := testDemand()
	d.RequestID = attempt.RequestID
	d.Workload = attempt.Workload
	if err := s.QueueDemand(ctx, d); err != nil {
		t.Fatal(err)
	}
	claim, err := s.ReserveDemand(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER reject_demand_attempt BEFORE INSERT ON comfyui_attempts BEGIN SELECT RAISE(ABORT,'test refusal'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdmitComfyUIWork(WithDemandClaim(ctx, claim), attempt, ""); err == nil {
		t.Fatal("metadata failure admitted demand")
	}
	if err := s.RequeueDemand(ctx, d.RequestID, claim); err != nil {
		t.Fatalf("partial demand admission: %v", err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER reject_demand_attempt`); err != nil {
		t.Fatal(err)
	}
	claim, err = s.ReserveDemand(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdmitComfyUIWork(WithDemandClaim(ctx, claim), attempt, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.RequeueDemand(ctx, d.RequestID, claim); !errors.Is(err, ErrDemandConflict) {
		t.Fatalf("admitted demand requeued: %v", err)
	}
}
