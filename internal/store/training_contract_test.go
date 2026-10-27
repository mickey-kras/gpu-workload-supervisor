package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func trainingContractStore(t *testing.T) (*Store, string, string) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	c := control.Catalog{Version: 4, Profiles: []control.WorkloadProfile{{ID: "trainer", Label: "Trainer", Adapter: "systemd", Unit: "trainer.service", Cgroup: "/training/trainer.service", RequiredMiB: 1024, BootPolicy: "stop-to-idle", Trainer: &control.TrainerBinding{Version: 1, GPUUUID: "GPU-11111111-1111-1111-1111-111111111111", LaunchFile: "/opt/trainer/unit.service", LaunchSHA256: strings.Repeat("a", 64), Executable: "/opt/trainer/train", ExecutableSHA256: strings.Repeat("b", 64), RecipeFile: "/opt/trainer/recipe.json", RecipeSHA256: strings.Repeat("c", 64), JobFile: "/opt/trainer/job.json", TerminalFile: "/opt/trainer/terminal.json"}}}}
	snap, err := s.ReplaceCatalog(ctx, "", c)
	if err != nil {
		t.Fatal(err)
	}
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state.Owner = control.OwnerSupervisor
	state.ActiveWorkload = control.WorkloadIdle
	state.DesiredWorkload = control.WorkloadIdle
	state.Phase = control.PhaseStable
	state.Health = control.HealthHealthy
	state.Admission = control.AdmissionClosed
	if _, err = s.UpdateState(ctx, state.Version, state); err != nil {
		t.Fatal(err)
	}
	return s, path, snap.Revision
}

func TestTrainingContractCompetingReservations(t *testing.T) {
	for _, sameID := range []bool{false, true} {
		t.Run(map[bool]string{false: "distinct", true: "duplicate"}[sameID], func(t *testing.T) {
			s, path, revision := trainingContractStore(t)
			other, err := Open(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			start := make(chan struct{})
			results := make(chan error, 2)
			var wg sync.WaitGroup
			for i, db := range []*Store{s, other} {
				wg.Add(1)
				go func(i int, db *Store) {
					defer wg.Done()
					<-start
					id := "job-a"
					if i == 1 && !sameID {
						id = "job-b"
					}
					_, err := reserveTrainingFixture(context.Background(), db, id, revision)
					results <- err
				}(i, db)
			}
			close(start)
			wg.Wait()
			close(results)
			success := 0
			for err := range results {
				if err == nil {
					success++
				}
			}
			if success != 1 {
				t.Fatalf("got %d successful reservations", success)
			}
			pending, err := s.PendingWork(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if pending != 1 {
				t.Fatalf("pending work=%d", pending)
			}
			state, err := s.State(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if state.Admission != control.AdmissionClosed || state.ActiveWorkload != "trainer" {
				t.Fatalf("invalid reserved state: %+v", state)
			}
		})
	}
}

func TestTrainingContractDurableIdentityAndNoReplay(t *testing.T) {
	s, path, revision := trainingContractStore(t)
	ctx := context.Background()
	job, err := reserveTrainingFixture(ctx, s, "job-a", revision)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecordTrainingLaunch(ctx, job, strings.Repeat("a", 32)); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	recorded, err := reopened.TrainingJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recorded.Status != "running" || recorded.Token != job.Token || recorded.InvocationID != strings.Repeat("a", 32) {
		t.Fatalf("lost durable identity: %+v", recorded)
	}
	if err = reopened.RecordTrainingLaunch(ctx, job, strings.Repeat("b", 32)); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("duplicate launch: %v", err)
	}
	for name, mutate := range map[string]func(*control.TrainingJob){"token": func(j *control.TrainingJob) { j.Token = "old" }, "epoch": func(j *control.TrainingJob) { j.Fence.Epoch-- }, "incarnation": func(j *control.TrainingJob) { j.Fence.Incarnation = "old" }} {
		t.Run(name, func(t *testing.T) {
			stale := job
			mutate(&stale)
			if _, err := s.FinishTraining(ctx, stale, "completed"); !errors.Is(err, ErrStaleFence) {
				t.Fatalf("stale completion: %v", err)
			}
		})
	}
	state, err := s.FinishTraining(ctx, job, "completed")
	if err != nil {
		t.Fatal(err)
	}
	if state.ActiveWorkload != control.WorkloadIdle || state.Admission != control.AdmissionClosed || state.LeaseFence.Epoch <= job.Fence.Epoch {
		t.Fatalf("invalid completion state: %+v", state)
	}
	if _, err = s.FinishTraining(ctx, job, "completed"); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("repeated completion: %v", err)
	}
	if _, err = reserveTrainingFixture(ctx, s, job.ID, revision); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("reused job identity: %v", err)
	}
	if _, err = reserveTrainingFixture(ctx, s, "job-b", revision); err != nil {
		t.Fatalf("new identity rejected: %v", err)
	}
}

func TestTrainingContractGenericPathsCannotCompleteTraining(t *testing.T) {
	for _, operation := range []string{"finish-work", "recover", "rotate-incarnation", "update-state"} {
		t.Run(operation, func(t *testing.T) {
			s, _, revision := trainingContractStore(t)
			ctx := context.Background()
			job, err := reserveTrainingFixture(ctx, s, "job-a", revision)
			if err != nil {
				t.Fatal(err)
			}
			state, err := s.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			switch operation {
			case "finish-work":
				err = s.FinishWorkToken(ctx, job.ID, job.Workload, job.Fence, job.Token, WorkCompleted)
			case "recover":
				final := state
				final.ActiveWorkload = control.WorkloadIdle
				final.DesiredWorkload = control.WorkloadIdle
				_, err = s.Recover(ctx, state.Version, final, "generic recovery")
			case "rotate-incarnation":
				_, err = s.RotateIncarnation(ctx)
			case "update-state":
				next := state
				next.ActiveWorkload = control.WorkloadIdle
				next.DesiredWorkload = control.WorkloadIdle
				_, err = s.UpdateState(ctx, state.Version, next)
			}
			if !errors.Is(err, ErrTrainingPending) {
				t.Fatalf("generic %s must reject pending training, got %v", operation, err)
			}
			pending, err := s.PendingWork(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if pending != 1 {
				t.Fatalf("generic operation completed training: %d", pending)
			}
		})
	}
}

func TestTrainingContractRetentionPreservesNonReplay(t *testing.T) {
	s, _, revision := trainingContractStore(t)
	ctx := context.Background()
	job, err := reserveTrainingFixture(ctx, s, "job-a", revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.FinishTraining(ctx, job, "failed"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PruneCompletedWork(ctx, time.Now().Add(time.Hour), 100); err != nil {
		t.Fatalf("retention must skip durable training identities: %v", err)
	}
	if _, err = reserveTrainingFixture(ctx, s, job.ID, revision); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("retention enabled replay: %v", err)
	}
}

func TestTrainingContractRejectsStaleCatalogAtomically(t *testing.T) {
	s, _, revision := trainingContractStore(t)
	ctx := context.Background()
	before, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reserveTrainingFixture(ctx, s, "job-a", "stale-revision"); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale revision: %v", err)
	}
	after, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before.Version != after.Version || before.LeaseFence != after.LeaseFence || after.ActiveWorkload != control.WorkloadIdle {
		t.Fatalf("failed reservation changed state: %+v", after)
	}
	job, err := reserveTrainingFixture(ctx, s, "job-a", revision)
	if err != nil {
		t.Fatalf("failed reservation consumed identity: %v", err)
	}
	snap, err := s.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	changed := snap.Catalog.Clone()
	changed.Profiles[0].Trainer.RecipeSHA256 = strings.Repeat("d", 64)
	if _, err = s.ReplaceCatalog(ctx, revision, changed); err == nil {
		t.Fatal("live trainer recipe replaced")
	}
	if _, err = s.FinishTraining(ctx, job, "running"); err == nil {
		t.Fatal("nonterminal outcome accepted")
	}
	pending, err := s.PendingWork(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatal("invalid outcome completed work")
	}
}

func TestTrainingContractRejectsControlStateOutputPaths(t *testing.T) {
	for _, suffix := range []string{"", "-wal", "-shm", "-journal", ".lock", ".proxy.lock", ".user-execution.lock", ".deployment.json"} {
		t.Run("state"+suffix, func(t *testing.T) {
			s, path, revision := trainingContractStore(t)
			ctx := context.Background()
			snap, err := s.Catalog(ctx)
			if err != nil {
				t.Fatal(err)
			}
			snap.Catalog.Profiles[0].Trainer.JobFile = path + suffix
			snap, err = s.ReplaceCatalog(ctx, revision, snap.Catalog)
			if err != nil {
				t.Fatal(err)
			}
			before, err := s.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = reserveTrainingFixture(ctx, s, "collision", snap.Revision); err == nil {
				t.Fatal("control state output accepted")
			}
			after, err := s.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if before.Version != after.Version || before.LeaseFence != after.LeaseFence {
				t.Fatal("rejected output changed control state")
			}
			if pending, err := s.PendingWork(ctx); err != nil || pending != 0 {
				t.Fatalf("rejected output admitted work: %d %v", pending, err)
			}
		})
	}
}

func TestTrainingContractTerminalReceiptIsFencedAndDurable(t *testing.T) {
	s, path, revision := trainingContractStore(t)
	ctx := context.Background()
	job, err := reserveTrainingFixture(ctx, s, "receipt-job", revision)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecordTrainingLaunch(ctx, job, strings.Repeat("a", 32)); err != nil {
		t.Fatal(err)
	}
	job, err = s.TrainingJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	evidence := control.TrainingTerminal{Version: 1, ID: job.ID, Token: job.Token, Fence: job.Fence, RecipeSHA256: job.RecipeSHA256, InvocationID: job.InvocationID, Outcome: "completed"}
	stale := evidence
	stale.Fence.Epoch--
	if err = s.RecordTrainingTerminal(ctx, job, stale); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("stale receipt: %v", err)
	}
	if err = s.RecordTrainingTerminal(ctx, job, evidence); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordTrainingTerminal(ctx, job, evidence); err != nil {
		t.Fatalf("identical receipt retry: %v", err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	stored, err := reopened.TrainingJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.TerminalOutcome != "completed" || stored.Status != "finishing" {
		t.Fatal("receipt lost on restart")
	}
	evidence.Outcome = "failed"
	if err = reopened.RecordTrainingTerminal(ctx, job, evidence); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("receipt overwritten: %v", err)
	}
	if _, err = reopened.FinishTraining(ctx, job, "interrupted"); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("receipt relabeled: %v", err)
	}
	if pending, err := reopened.PendingWork(ctx); err != nil || pending != 1 {
		t.Fatalf("receipt completed admission: %d %v", pending, err)
	}
	if _, err = reopened.FinishTraining(ctx, job, "completed"); err != nil {
		t.Fatal(err)
	}
}

func TestTrainingContractRejectsCanonicalStateOutputThroughAncestorAlias(t *testing.T) {
	original, path, _ := trainingContractStore(t)
	ctx := context.Background()
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	parent := filepath.Dir(path)
	if err := os.Symlink(filepath.Dir(parent), alias); err != nil {
		t.Fatal(err)
	}
	aliased := filepath.Join(alias, filepath.Base(parent), filepath.Base(path))
	s, err := Open(ctx, aliased)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	snap, err := s.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snap.Catalog.Profiles[0].Trainer.JobFile = path
	snap, err = s.ReplaceCatalog(ctx, snap.Revision, snap.Catalog)
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reserveTrainingFixture(ctx, s, "alias-job", snap.Revision); err == nil {
		t.Fatal("canonical state path bypassed output guard")
	}
	after, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before.Version != after.Version || before.LeaseFence != after.LeaseFence {
		t.Fatal("rejected alias changed state")
	}
	if pending, err := s.PendingWork(ctx); err != nil || pending != 0 {
		t.Fatalf("rejected alias admitted work: %d %v", pending, err)
	}
}

func TestTrainingContractDoesNotClaimDemandIdentity(t *testing.T) {
	for _, reserved := range []bool{false, true} {
		t.Run(map[bool]string{false: "queued", true: "reserved"}[reserved], func(t *testing.T) {
			ctx := context.Background()
			s, _, revision := trainingContractStore(t)
			d := testDemand()
			if err := s.QueueDemand(ctx, d); err != nil {
				t.Fatal(err)
			}
			if reserved {
				if _, err := s.ReserveDemand(ctx, d); err != nil {
					t.Fatal(err)
				}
			}
			before, err := s.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := reserveTrainingFixture(ctx, s, d.RequestID, revision); !errors.Is(err, ErrRequestConflict) {
				t.Fatalf("demand identity claimed: %v", err)
			}
			after, err := s.State(ctx)
			if err != nil || before != after {
				t.Fatalf("state changed: %v", err)
			}
			if n, err := s.PendingWork(ctx); err != nil || n != 0 {
				t.Fatalf("unexpected admission: %d %v", n, err)
			}
			var status string
			if err := s.db.QueryRow(`SELECT status FROM broker_demands WHERE request_id=?`, d.RequestID).Scan(&status); err != nil {
				t.Fatal(err)
			}
			want := "queued"
			if reserved {
				want = "reserved"
			}
			if status != want {
				t.Fatalf("demand=%s", status)
			}
		})
	}
}

func TestTrainingContractMigrationPreservesV16Demand(t *testing.T) {
	ctx := context.Background()
	s, path, revision := trainingContractStore(t)
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
	if _, err := s.db.Exec(`DROP TABLE training_claims; DROP TABLE training_jobs; DELETE FROM schema_migrations WHERE version=17`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	after, err := upgraded.State(ctx)
	if err != nil || before != after {
		t.Fatalf("migration changed state: %v", err)
	}
	var got, status string
	if err := upgraded.db.QueryRow(`SELECT claim,status FROM broker_demands WHERE request_id=?`, d.RequestID).Scan(&got, &status); err != nil || got != claim || status != "reserved" {
		t.Fatalf("demand changed: %s %s %v", got, status, err)
	}
	if _, err := reserveTrainingFixture(ctx, upgraded, "distinct-job", revision); err != nil {
		t.Fatal(err)
	}
}

func TestTrainingContractReservationWriteFailureRollsBack(t *testing.T) {
	for _, target := range []struct{ table, action string }{{"registered_work", "INSERT"}, {"training_jobs", "INSERT"}, {"training_claims", "UPDATE"}, {"idle_policy_state", "UPDATE"}, {"control_state", "UPDATE"}} {
		t.Run(target.table, func(t *testing.T) {
			ctx := context.Background()
			s, _, revision := trainingContractStore(t)
			before, err := s.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			claim, err := s.ClaimTraining(ctx, "job-write-failure", "trainer", revision)
			if err != nil {
				t.Fatal(err)
			}
			statement := "CREATE TRIGGER reject_training_write BEFORE " + target.action + " ON " + target.table + " BEGIN SELECT RAISE(ABORT, 'injected persistence failure'); END"
			if _, err := s.db.Exec(statement); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ReserveTraining(ctx, claim); err == nil {
				t.Fatal("failed durable write accepted")
			}
			after, err := s.State(ctx)
			if err != nil || before != after {
				t.Fatalf("partial authority mutation: %v", err)
			}
			if n, err := s.PendingWork(ctx); err != nil || n != 0 {
				t.Fatalf("partial admission: %d %v", n, err)
			}
			if saved, err := s.TrainingJob(ctx, "job-write-failure"); err != nil || saved.Status != "preparing" {
				t.Fatalf("partial job: %v", err)
			}
			if _, err := s.db.Exec("DROP TRIGGER reject_training_write"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ReserveTraining(ctx, claim); err != nil {
				t.Fatalf("rollback did not preserve retry: %v", err)
			}
		})
	}
}

func TestTrainingContractFinishWriteFailurePreservesAdmission(t *testing.T) {
	for _, table := range []string{"training_jobs", "registered_work", "idle_policy_state", "control_state"} {
		t.Run(table, func(t *testing.T) {
			ctx := context.Background()
			s, _, revision := trainingContractStore(t)
			job, err := reserveTrainingFixture(ctx, s, "job-finish-failure", revision)
			if err != nil {
				t.Fatal(err)
			}
			before, err := s.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec("CREATE TRIGGER reject_training_write BEFORE UPDATE ON " + table + " BEGIN SELECT RAISE(ABORT, 'injected persistence failure'); END"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.FinishTraining(ctx, job, "interrupted"); err == nil {
				t.Fatal("failed finalization accepted")
			}
			after, err := s.State(ctx)
			if err != nil || before != after {
				t.Fatalf("partial authority mutation: %v", err)
			}
			if n, err := s.PendingWork(ctx); err != nil || n != 1 {
				t.Fatalf("lost admission: %d %v", n, err)
			}
			saved, err := s.TrainingJob(ctx, job.ID)
			if err != nil || saved != job {
				t.Fatalf("partial terminal mutation: %v", err)
			}
			if _, err := s.db.Exec("DROP TRIGGER reject_training_write"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.FinishTraining(ctx, job, "interrupted"); err != nil {
				t.Fatalf("finalization retry: %v", err)
			}
			if n, err := s.PendingWork(ctx); err != nil || n != 0 {
				t.Fatalf("retry did not finalize: %d %v", n, err)
			}
		})
	}
}

func TestTrainingContractRejectsInvalidAndMissingIdentity(t *testing.T) {
	ctx := context.Background()
	s, _, revision := trainingContractStore(t)
	if _, err := reserveTrainingFixture(ctx, s, "", revision); err == nil {
		t.Fatal("empty job ID accepted")
	}
	if err := s.CheckTrainingRequestID(ctx, ""); err == nil {
		t.Fatal("empty preflight ID accepted")
	}
	missing := control.TrainingJob{ID: "absent-job"}
	if err := s.RecordTrainingLaunch(ctx, missing, strings.Repeat("a", 32)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing launch: %v", err)
	}
	if err := s.RecordTrainingTerminal(ctx, missing, control.TrainingTerminal{}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing receipt: %v", err)
	}
	if _, err := s.FinishTraining(ctx, missing, "completed"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing finish: %v", err)
	}
	job, err := reserveTrainingFixture(ctx, s, "valid-job", revision)
	if err != nil {
		t.Fatal(err)
	}
	for _, invocation := range []string{"", "not-hex", "abcd"} {
		if err := s.RecordTrainingLaunch(ctx, job, invocation); err == nil {
			t.Fatal("invalid invocation accepted")
		}
	}
	saved, err := s.TrainingJob(ctx, job.ID)
	if err != nil || saved != job {
		t.Fatalf("invalid invocation changed durable job: %v", err)
	}
	invocation := strings.Repeat("a", 32)
	if err := s.RecordTrainingLaunch(ctx, job, invocation); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTrainingLaunch(ctx, job, strings.Repeat("b", 32)); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("second invocation replaced first: %v", err)
	}
	saved, err = s.TrainingJob(ctx, job.ID)
	if err != nil || saved.InvocationID != invocation {
		t.Fatalf("launch identity changed: %v", err)
	}
	if n, err := s.PendingWork(ctx); err != nil || n != 1 {
		t.Fatalf("invalid mutation lost pending admission: %d %v", n, err)
	}
}

func reserveTrainingFixture(ctx context.Context, s *Store, id, revision string) (control.TrainingJob, error) {
	claim, err := s.ClaimTraining(ctx, id, "trainer", revision)
	if err != nil {
		return claim, err
	}
	return s.ReserveTraining(ctx, claim)
}

func TestTrainingContractMemoryPeakKeepsHighWaterMarkDurably(t *testing.T) {
	s, path, revision := trainingContractStore(t)
	ctx := context.Background()
	job, err := reserveTrainingFixture(ctx, s, "peak-job", revision)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecordTrainingMemoryPeak(ctx, job, 1); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("recorded a peak before launch evidence: %v", err)
	}
	if err = s.RecordTrainingLaunch(ctx, job, strings.Repeat("a", 32)); err != nil {
		t.Fatal(err)
	}
	stale := job
	stale.Fence.Epoch--
	if err = s.RecordTrainingMemoryPeak(ctx, stale, 1); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("stale peak: %v", err)
	}
	for _, peak := range []uint64{4 << 30, 5 << 30, 2 << 30} {
		if err = s.RecordTrainingMemoryPeak(ctx, job, peak); err != nil {
			t.Fatal(err)
		}
	}
	// The smaller last value does not replace the high-water mark.
	if stored, err := s.TrainingJob(ctx, job.ID); err != nil || stored.MemoryPeakBytes == nil || *stored.MemoryPeakBytes != 5<<30 {
		t.Fatalf("job = %+v, %v", stored, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	stored, err := reopened.TrainingJob(ctx, job.ID)
	if err != nil || stored.MemoryPeakBytes == nil || *stored.MemoryPeakBytes != 5<<30 || stored.Status != "running" {
		t.Fatalf("job = %+v, %v", stored, err)
	}
	if _, err = reopened.FinishTraining(ctx, job, "failed"); err != nil {
		t.Fatal(err)
	}
	if err = reopened.RecordTrainingMemoryPeak(ctx, job, 6<<30); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("changed the peak of a finished job: %v", err)
	}
}
