package store

import (
	"context"
	"errors"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestTrainingClaimRecoverySurvivesReopenWithoutAdmission(t *testing.T) {
	ctx := context.Background()
	s, path, revision := trainingContractStore(t)
	before, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := s.ClaimTraining(ctx, "prepared", "trainer", revision)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.PendingWork(ctx); err != nil || n != 0 {
		t.Fatalf("claim became GPU admission: %d %v", n, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	saved, err := reopened.TrainingJob(ctx, claim.ID)
	if err != nil || saved != claim {
		t.Fatalf("claim lost: %v", err)
	}
	after, err := reopened.AbortTrainingPreparation(ctx, saved)
	if err != nil || after != before {
		t.Fatalf("recovery changed GPU state: %v", err)
	}
	saved, err = reopened.TrainingJob(ctx, claim.ID)
	if err != nil || saved.Status != "interrupted" {
		t.Fatalf("recovery outcome: %v", err)
	}
	if _, err := reopened.ClaimTraining(ctx, claim.ID, "trainer", revision); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("replayed aborted claim: %v", err)
	}
}

func TestTrainingClaimCannotBeStolenOrAbortConsumedAdmission(t *testing.T) {
	ctx := context.Background()
	s, _, revision := trainingContractStore(t)
	claim, err := s.ClaimTraining(ctx, "prepared", "trainer", revision)
	if err != nil {
		t.Fatal(err)
	}
	forged := claim
	forged.Token = "other-token"
	if _, err := s.ReserveTraining(ctx, forged); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("forged reservation: %v", err)
	}
	if _, err := s.AbortTrainingPreparation(ctx, forged); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("forged abort: %v", err)
	}
	job, err := s.ReserveTraining(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AbortTrainingPreparation(ctx, claim); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("consumed claim aborted: %v", err)
	}
	saved, err := s.TrainingJob(ctx, job.ID)
	if err != nil || saved != job {
		t.Fatalf("consumed admission changed: %v", err)
	}
	if n, err := s.PendingWork(ctx); err != nil || n != 1 {
		t.Fatalf("lost admission: %d %v", n, err)
	}
}

func TestTrainingClaimBlocksGenericAndBrokerAdmission(t *testing.T) {
	ctx := context.Background()
	s, _, _ := trainingContractStore(t)
	snap, err := s.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	catalog := snap.Catalog.Clone()
	catalog.Profiles = append(catalog.Profiles, control.WorkloadProfile{ID: control.WorkloadText, Label: "Text", Adapter: "systemd", Unit: "text.service", Cgroup: "/text", HealthURL: "http://localhost:8080/health"})
	snap, err = s.ReplaceCatalog(ctx, snap.Revision, catalog)
	if err != nil {
		t.Fatal(err)
	}
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state.ActiveWorkload = control.WorkloadText
	state.DesiredWorkload = control.WorkloadText
	state.Admission = control.AdmissionOpen
	state, err = s.UpdateState(ctx, state.Version, state)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := s.ClaimTraining(ctx, "prepared", "trainer", snap.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdmitWorkToken(ctx, claim.ID, "", control.WorkloadText, state.LeaseFence); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("generic work stole claim: %v", err)
	}
	d := testDemand()
	d.RequestID = claim.ID
	if err := s.QueueDemand(ctx, d); !errors.Is(err, ErrDemandConflict) {
		t.Fatalf("demand stole claim: %v", err)
	}
	if n, err := s.PendingWork(ctx); err != nil || n != 0 {
		t.Fatalf("rejected admission persisted: %d %v", n, err)
	}
	after, err := s.State(ctx)
	if err != nil || after != state {
		t.Fatalf("claim changed inference: %v", err)
	}
}

func TestTrainingClaimAndDemandRaceHasOneIdentityOwner(t *testing.T) {
	ctx := context.Background()
	s, path, revision := trainingContractStore(t)
	other, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	for _, id := range []string{"race-one", "race-two", "race-three", "race-four"} {
		start := make(chan struct{})
		results := make(chan error, 2)
		go func() { <-start; _, err := s.ClaimTraining(ctx, id, "trainer", revision); results <- err }()
		go func() { <-start; d := testDemand(); d.RequestID = id; results <- other.QueueDemand(ctx, d) }()
		close(start)
		successful := 0
		for range 2 {
			if err := <-results; err == nil {
				successful++
			}
		}
		if successful != 1 {
			t.Fatalf("successful identity owners=%d", successful)
		}
		var owners int
		if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM training_claims WHERE id=?)+(SELECT COUNT(*) FROM broker_demands WHERE request_id=?)`, id, id).Scan(&owners); err != nil || owners != 1 {
			t.Fatalf("durable identity owners=%d error=%v", owners, err)
		}
	}
	if n, err := s.PendingWork(ctx); err != nil || n != 0 {
		t.Fatalf("identity race admitted GPU work: %d %v", n, err)
	}
}

func TestTrainingClaimInsertFailureHasNoIdentityOrAuthorityEffect(t *testing.T) {
	ctx := context.Background()
	s, _, revision := trainingContractStore(t)
	before, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER reject_claim BEFORE INSERT ON training_claims BEGIN SELECT RAISE(ABORT,'claim persistence unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimTraining(ctx, "failed-claim", "trainer", revision); err == nil {
		t.Fatal("failed claim accepted")
	}
	if err := s.CheckTrainingRequestID(ctx, "failed-claim"); err != nil {
		t.Fatalf("failed write consumed identity: %v", err)
	}
	after, err := s.State(ctx)
	if err != nil || after != before {
		t.Fatalf("failed claim changed state: %v", err)
	}
	if n, err := s.PendingWork(ctx); err != nil || n != 0 {
		t.Fatalf("failed claim admitted GPU work: %d %v", n, err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER reject_claim`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimTraining(ctx, "failed-claim", "trainer", revision); err != nil {
		t.Fatalf("failed claim not retryable: %v", err)
	}
}
