package supervisor

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

type trainingContractRuntime struct {
	fakeRuntime
	terminal      control.TrainingTerminal
	terminalErr   error
	releaseErr    error
	trainingCalls []string
	startErr      error
	startHook     func(control.TrainingJob)
	invocation    string
}

func (r *trainingContractRuntime) StartTraining(_ context.Context, _ control.WorkloadProfile, job control.TrainingJob) (string, error) {
	r.trainingCalls = append(r.trainingCalls, "launch")
	if r.startHook != nil {
		r.startHook(job)
	}
	return r.invocation, r.startErr
}
func (r *trainingContractRuntime) TrainingTerminal(context.Context, control.WorkloadProfile, control.TrainingJob) (control.TrainingTerminal, error) {
	r.trainingCalls = append(r.trainingCalls, "terminal")
	return r.terminal, r.terminalErr
}
func (r *trainingContractRuntime) ReleaseTraining(context.Context, control.WorkloadProfile) error {
	r.trainingCalls = append(r.trainingCalls, "release")
	return r.releaseErr
}

func trainingControllerContract(t *testing.T) (*Controller, *store.Store, *trainingContractRuntime, control.TrainingJob) {
	t.Helper()
	ctx := context.Background()
	s := openStore(t)
	c := control.Catalog{Version: 4, Profiles: []control.WorkloadProfile{{ID: "trainer", Label: "Trainer", Adapter: "systemd", Unit: "trainer.service", Cgroup: "/training/trainer.service", RequiredMiB: 1024, BootPolicy: "stop-to-idle", Trainer: &control.TrainerBinding{Version: 1, GPUUUID: "GPU-11111111-1111-1111-1111-111111111111", LaunchFile: "/opt/trainer/unit.service", LaunchSHA256: strings.Repeat("a", 64), Executable: "/opt/trainer/train", ExecutableSHA256: strings.Repeat("b", 64), RecipeFile: "/opt/trainer/recipe.json", RecipeSHA256: strings.Repeat("c", 64), JobFile: "/opt/trainer/job.json", TerminalFile: "/opt/trainer/terminal.json"}}}}
	snapshot, err := s.ReplaceCatalog(ctx, "", c)
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
	job, err := reserveTrainingFixture(ctx, s, "job-a", snapshot.Revision)
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
	r := &trainingContractRuntime{terminal: control.TrainingTerminal{Version: 1, ID: job.ID, Token: job.Token, Fence: job.Fence, RecipeSHA256: job.RecipeSHA256, InvocationID: job.InvocationID, Outcome: "completed"}}
	controller := testController(t, s, r)
	controller.config.VerifyTimeout = 5 * time.Millisecond
	return controller, s, r, job
}

func TestTrainingContractReconcileRequiresTerminalAndRelease(t *testing.T) {
	for _, failure := range []string{"missing-terminal", "stale-terminal", "stop-failed", "release-failed", "none"} {
		t.Run(failure, func(t *testing.T) {
			c, s, r, job := trainingControllerContract(t)
			ctx := context.Background()
			switch failure {
			case "missing-terminal":
				r.terminalErr = errors.New("missing terminal")
			case "stale-terminal":
				r.terminal.Fence.Epoch--
			case "stop-failed":
				r.stopErr = errors.New("stop failed")
			case "release-failed":
				r.releaseErr = errors.New("memory remains allocated")
			}
			_, err := c.ReconcileTraining(ctx, job.ID)
			if (err == nil) != (failure == "none") {
				t.Fatalf("reconcile error: %v", err)
			}
			pending, err := s.PendingWork(ctx)
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if failure == "none" {
				want = 0
			}
			if pending != want {
				t.Fatalf("pending=%d want=%d", pending, want)
			}
			for _, call := range r.trainingCalls {
				if call == "launch" {
					t.Fatal("reconciliation replayed trainer")
				}
			}
			if (failure == "missing-terminal" || failure == "stale-terminal") && len(r.calls) != 0 {
				t.Fatalf("invalid terminal caused side effects: %v", r.calls)
			}
			if failure == "stop-failed" {
				for _, call := range r.trainingCalls {
					if call == "release" {
						t.Fatal("release checked after failed stop")
					}
				}
			}
			if failure == "none" {
				stored, err := s.TrainingJob(ctx, job.ID)
				if err != nil {
					t.Fatal(err)
				}
				if stored.Status != "completed" {
					t.Fatalf("status=%s", stored.Status)
				}
			}
		})
	}
}

func TestTrainingContractRecoveryNeverReplaysOrClaimsSuccess(t *testing.T) {
	c, s, r, job := trainingControllerContract(t)
	ctx := context.Background()
	r.terminalErr = errors.New("lost terminal")
	// A fresh controller models restart with only durable store identity.
	c = testController(t, s, r)
	if _, err := c.RecoverTraining(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := s.TrainingJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "interrupted" {
		t.Fatalf("recovery status=%s", stored.Status)
	}
	assertCalls(t, r.calls, "stop trainer")
	assertCalls(t, r.trainingCalls, "release")
	if _, err := c.RecoverTraining(ctx, job.ID); !errors.Is(err, store.ErrStaleFence) {
		t.Fatalf("repeated recovery: %v", err)
	}
}

// Begin from a completed predecessor so the fixture also exercises a second job.
func trainingStartContract(t *testing.T) (*Controller, *store.Store, *trainingContractRuntime) {
	t.Helper()
	c, s, r, job := trainingControllerContract(t)
	if _, err := s.FinishTraining(context.Background(), job, "completed"); err != nil {
		t.Fatal(err)
	}
	r.invocation = strings.Repeat("b", 32)
	return c, s, r
}

type trainingRecordFailureStore struct {
	*store.Store
	recordErr error
}

func (s *trainingRecordFailureStore) RecordTrainingLaunch(context.Context, control.TrainingJob, string) error {
	return s.recordErr
}

func TestTrainingContractStartPersistsIntentAndNeverRetries(t *testing.T) {
	for _, failure := range []string{"none", "cancelled", "ambiguous", "record-failed"} {
		t.Run(failure, func(t *testing.T) {
			c, s, r := trainingStartContract(t)
			ctx := context.Background()
			sentinel := errors.New("launch acknowledgement failed")
			switch failure {
			case "cancelled":
				r.startErr = context.Canceled
				r.invocation = ""
			case "ambiguous":
				r.startErr = sentinel
			case "record-failed":
				c = testController(t, &trainingRecordFailureStore{s, sentinel}, r)
			}
			r.startHook = func(job control.TrainingJob) {
				persisted, err := s.TrainingJob(ctx, job.ID)
				if err != nil {
					t.Fatal(err)
				}
				if persisted.Status != "launching" || persisted.Token != job.Token || persisted.Fence != job.Fence {
					t.Fatalf("effect precedes committed intent: %+v", persisted)
				}
				state, err := s.State(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if state.Admission != control.AdmissionClosed || state.ActiveWorkload != "trainer" {
					t.Fatalf("launch not exclusively reserved: %+v", state)
				}
			}
			_, err := c.StartTraining(ctx, "job-b", "trainer")
			if (err == nil) != (failure == "none") {
				t.Fatalf("start error=%v", err)
			}
			saved, err := s.TrainingJob(ctx, "job-b")
			if err != nil {
				t.Fatal(err)
			}
			want := "running"
			if failure == "cancelled" || failure == "record-failed" {
				want = "launching"
			}
			if saved.Status != want {
				t.Fatalf("status=%s want=%s", saved.Status, want)
			}
			pending, err := s.PendingWork(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if pending != 1 {
				t.Fatal("launch uncertainty cleared admission")
			}
			restarted := testController(t, s, r)
			if _, err = restarted.StartTraining(ctx, "job-b", "trainer"); !errors.Is(err, store.ErrRequestConflict) {
				t.Fatalf("replay after restart: %v", err)
			}
			assertCalls(t, r.trainingCalls, "launch")
		})
	}
}

func TestTrainingContractMaximumRequestIDsReconcileDurably(t *testing.T) {
	for name, id := range map[string]string{
		"plain":   strings.Repeat("x", control.MaxRequestIDBytes),
		"escaped": strings.Repeat("<", control.MaxRequestIDBytes),
		"UTF-8":   strings.Repeat("é", control.MaxRequestIDBytes/2),
	} {
		t.Run(name, func(t *testing.T) {
			c, s, r := trainingStartContract(t)
			ctx := context.Background()
			job, err := c.StartTraining(ctx, id, "trainer")
			if err != nil {
				t.Fatal(err)
			}
			if job.ID != id || job.Status != "running" {
				t.Fatal("start did not preserve the accepted request ID")
			}
			r.terminal = control.TrainingTerminal{Version: 1, ID: job.ID, Token: job.Token, Fence: job.Fence, RecipeSHA256: job.RecipeSHA256, InvocationID: job.InvocationID, Outcome: "completed"}
			// Fail cleanup after the receipt is persisted, then reconcile from a
			// fresh controller without access to the retained invocation.
			r.stopErr = errors.New("cleanup unavailable")
			if _, err := c.ReconcileTraining(ctx, id); err == nil {
				t.Fatal("cleanup failure not reported")
			}
			recorded, err := s.TrainingJob(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if recorded.ID != id || recorded.Status != "finishing" || recorded.TerminalOutcome != "completed" {
				t.Fatal("accepted request ID or completed receipt was not persisted")
			}
			if pending, err := s.PendingWork(ctx); err != nil || pending != 1 {
				t.Fatalf("pending before release=%d, %v", pending, err)
			}
			r.stopErr = nil
			r.terminalErr = errors.New("invocation no longer retained")
			r.trainingCalls = nil
			fresh := testController(t, s, r)
			if _, err := fresh.ReconcileTraining(ctx, id); err != nil {
				t.Fatal(err)
			}
			recorded, err = s.TrainingJob(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if recorded.ID != id || recorded.Status != "completed" || recorded.TerminalOutcome != "completed" {
				t.Fatal("reconciliation lost the accepted request ID or completed outcome")
			}
			if pending, err := s.PendingWork(ctx); err != nil || pending != 0 {
				t.Fatalf("pending after reconciliation=%d, %v", pending, err)
			}
			assertCalls(t, r.trainingCalls, "release")
		})
	}
}

func TestTrainingContractStartRejectsStaleCatalogBeforeEffect(t *testing.T) {
	c, s, r := trainingStartContract(t)
	ctx := context.Background()
	snap, err := s.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	updated := snap.Catalog.Clone()
	updated.Profiles[0].Label = "Updated trainer"
	if _, err = s.ReplaceCatalog(ctx, snap.Revision, updated); err != nil {
		t.Fatal(err)
	}
	if _, err = c.StartTraining(ctx, "job-b", "trainer"); !errors.Is(err, store.ErrVersionConflict) {
		t.Fatalf("stale start=%v", err)
	}
	if len(r.trainingCalls) != 0 {
		t.Fatalf("stale catalog launched: %v", r.trainingCalls)
	}
}

func TestTrainingContractStartDrainsOpposingWork(t *testing.T) {
	_, s, r := trainingStartContract(t)
	ctx := context.Background()
	snap, err := s.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	catalog := snap.Catalog.Clone()
	catalog.Profiles = append(catalog.Profiles, control.WorkloadProfile{ID: control.WorkloadText, Label: "Text", Adapter: "systemd", Unit: "text.service", Cgroup: "/text", HealthURL: "http://localhost:8080/health"})
	if _, err = s.ReplaceCatalog(ctx, snap.Revision, catalog); err != nil {
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
	token, err := s.AdmitWorkToken(ctx, "opposing-request", "", control.WorkloadText, state.LeaseFence)
	if err != nil {
		t.Fatal(err)
	}
	r.active = control.WorkloadText
	c := testController(t, s, r)
	drainResult := make(chan error, 1)
	go func() {
		deadline := time.NewTimer(time.Second)
		defer deadline.Stop()
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-deadline.C:
				drainResult <- errors.New("admission never closed")
				return
			case <-ticker.C:
				observed, err := s.State(ctx)
				if err != nil {
					drainResult <- err
					return
				}
				if observed.Admission == control.AdmissionClosed {
					demand := store.Demand{RequestID: "job-b", Requester: "racer", Workflow: "inference", BodySHA256: strings.Repeat("d", 64), Workload: control.WorkloadText}
					if err := s.QueueDemand(ctx, demand); !errors.Is(err, store.ErrDemandConflict) {
						drainResult <- errors.New("broker stole training identity during drain")
						return
					}
					drainResult <- s.FinishWorkToken(ctx, "opposing-request", control.WorkloadText, state.LeaseFence, token, store.WorkCompleted)
					return
				}
			}
		}
	}()
	r.startHook = func(control.TrainingJob) {
		if r.active == control.WorkloadText {
			t.Fatal("trainer launched before opposing runtime stopped")
		}
	}
	_, startErr := c.StartTraining(ctx, "job-b", "trainer")
	if err := <-drainResult; err != nil {
		t.Fatal(err)
	}
	if startErr != nil {
		t.Fatal(startErr)
	}
	assertCalls(t, r.calls, "stop trainer", "stop text")
	assertCalls(t, r.trainingCalls, "launch")
}

func TestTrainingContractUnsupportedRuntimeHasNoEffects(t *testing.T) {
	_, s, _, job := trainingControllerContract(t)
	r := &fakeRuntime{}
	c := testController(t, s, r)
	ctx := context.Background()
	if _, err := c.TrainingStatus(ctx, job.ID); err == nil {
		t.Fatal("unsupported status accepted")
	}
	if _, err := c.StartTraining(ctx, "new-job", "trainer"); err == nil {
		t.Fatal("unsupported launch accepted")
	}
	if _, err := c.ReconcileTraining(ctx, job.ID); err == nil {
		t.Fatal("unsupported reconciliation accepted")
	}
	if len(r.calls) != 0 {
		t.Fatalf("unsupported capability caused effects: %v", r.calls)
	}
}

func TestTrainingContractBadIdentityAndMissingJobHaveNoEffects(t *testing.T) {
	c, s, r := trainingStartContract(t)
	ctx := context.Background()
	if _, err := c.StartTraining(ctx, "", "trainer"); err == nil {
		t.Fatal("empty identity accepted")
	}
	if _, err := c.StartTraining(ctx, "job-b", "absent"); err == nil {
		t.Fatal("missing profile accepted")
	}
	if _, err := c.ReconcileTraining(ctx, "missing"); err == nil {
		t.Fatal("missing job reconciled")
	}
	if _, err := c.TrainingStatus(ctx, "job-a"); err != nil {
		t.Fatal(err)
	}
	if len(r.trainingCalls) != 0 || len(r.calls) != 0 {
		t.Fatal("invalid request reached runtime")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.StartTraining(ctx, "job-b", "trainer"); err == nil {
		t.Fatal("database error ignored")
	}
}

func TestTrainingContractReconcileRejectsChangedCatalogAndStateReadFailure(t *testing.T) {
	for _, failure := range []string{"catalog", "state-read"} {
		t.Run(failure, func(t *testing.T) {
			c, s, r, job := trainingControllerContract(t)
			ctx := context.Background()
			if failure == "catalog" {
				c.config.Catalog.Revision = "other"
			} else {
				c.store = &trainingStateFailureStore{Store: s}
			}
			if _, err := c.ReconcileTraining(ctx, job.ID); err == nil {
				t.Fatal("unverifiable context reconciled")
			}
			if len(r.trainingCalls) != 0 || len(r.calls) != 0 {
				t.Fatal("unverifiable context reached runtime")
			}
		})
	}
}

type trainingStateFailureStore struct{ *store.Store }

func (s *trainingStateFailureStore) State(context.Context) (control.State, error) {
	return control.State{}, errors.New("state unavailable")
}

type trainingFinishFailureStore struct{ *store.Store }

func (s trainingFinishFailureStore) FinishTraining(context.Context, control.TrainingJob, string) (control.State, error) {
	return control.State{}, errors.New("terminal transaction unavailable")
}

type trainingReceiptFailureStore struct{ *store.Store }

func (s trainingReceiptFailureStore) RecordTrainingTerminal(context.Context, control.TrainingJob, control.TrainingTerminal) error {
	return errors.New("receipt transaction unavailable")
}

func TestTrainingContractTerminalReceiptSurvivesCleanupFailure(t *testing.T) {
	for _, outcome := range []string{"completed", "failed", "cancelled"} {
		for _, failure := range []string{"stop", "release", "finalization"} {
			t.Run(outcome+"/"+failure, func(t *testing.T) {
				c, s, r, job := trainingControllerContract(t)
				ctx := context.Background()
				r.terminal.Outcome = outcome
				switch failure {
				case "stop":
					r.stopErr = context.Canceled
				case "release":
					r.releaseErr = errors.New("GPU still allocated")
				case "finalization":
					c.store = trainingFinishFailureStore{s}
				}
				if _, err := c.ReconcileTraining(ctx, job.ID); err == nil {
					t.Fatal("failure not reported")
				}
				recorded, err := s.TrainingJob(ctx, job.ID)
				if err != nil {
					t.Fatal(err)
				}
				if recorded.TerminalOutcome != outcome || recorded.Status != "finishing" {
					t.Fatalf("lost terminal proof: %+v", recorded)
				}
				if pending, err := s.PendingWork(ctx); err != nil || pending != 1 {
					t.Fatalf("premature admission completion: %d %v", pending, err)
				}
				// A fresh controller cannot rely on any prior runtime observation. Stopping
				// a retained service has removed the identity that supported the receipt.
				r.stopErr = nil
				r.releaseErr = nil
				r.terminalErr = errors.New("invocation no longer retained")
				r.trainingCalls = nil
				fresh := testController(t, s, r)
				if failure == "finalization" {
					_, err = fresh.RecoverTraining(ctx, job.ID)
				} else {
					_, err = fresh.ReconcileTraining(ctx, job.ID)
				}
				if err != nil {
					t.Fatal(err)
				}
				recorded, err = s.TrainingJob(ctx, job.ID)
				if err != nil {
					t.Fatal(err)
				}
				if recorded.Status != outcome {
					t.Fatalf("acknowledged outcome changed to %s", recorded.Status)
				}
				for _, call := range r.trainingCalls {
					if call == "terminal" || call == "launch" {
						t.Fatalf("retry needed destroyed evidence or replayed: %v", r.trainingCalls)
					}
				}
			})
		}
	}
}

func TestTrainingContractReceiptWriteFailurePrecedesCleanup(t *testing.T) {
	c, s, r, job := trainingControllerContract(t)
	c.store = trainingReceiptFailureStore{s}
	if _, err := c.ReconcileTraining(context.Background(), job.ID); err == nil {
		t.Fatal("receipt failure ignored")
	}
	if len(r.calls) != 0 {
		t.Fatalf("cleanup ran without durable receipt: %v", r.calls)
	}
	recorded, err := s.TrainingJob(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recorded.TerminalOutcome != "" {
		t.Fatal("failed transaction wrote a receipt")
	}
}

func TestTrainingContractRoutineTransitionsWaitForRelease(t *testing.T) {
	for _, phase := range []string{"launching", "running", "awaiting-release"} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			c, s, r := trainingStartContract(t)
			job, err := reserveTrainingFixture(ctx, s, "job-b", c.config.Catalog.Revision)
			if err != nil {
				t.Fatal(err)
			}
			if phase != "launching" {
				if err := s.RecordTrainingLaunch(ctx, job, r.invocation); err != nil {
					t.Fatal(err)
				}
				job, err = s.TrainingJob(ctx, job.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			r.terminal = control.TrainingTerminal{Version: 1, ID: job.ID, Token: job.Token, Fence: job.Fence, RecipeSHA256: job.RecipeSHA256, InvocationID: job.InvocationID, Outcome: "completed"}
			if phase == "awaiting-release" {
				r.releaseErr = errors.New("GPU allocation remains")
				if _, err := c.ReconcileTraining(ctx, job.ID); err == nil {
					t.Fatal("release failure accepted")
				}
			}
			// Requester demand is durable intent, not permission to displace training.
			demand := store.Demand{RequestID: "requester-job", Requester: "test", Workflow: "inference", BodySHA256: strings.Repeat("d", 64), Workload: control.WorkloadText}
			if err := s.QueueDemand(ctx, demand); err != nil {
				t.Fatal(err)
			}
			claim, err := s.ReserveDemand(ctx, demand)
			if err != nil {
				t.Fatal(err)
			}
			ctx = store.WithDemandClaim(ctx, claim)
			r.calls, r.trainingCalls = nil, nil
			before, err := s.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.Switch(ctx, control.WorkloadIdle, "operator"); !errors.Is(err, store.ErrTrainingPending) {
				t.Fatalf("switch error=%v", err)
			}
			if _, err := c.TransferToUser(ctx, control.WorkloadIdle, "operator"); !errors.Is(err, store.ErrTrainingPending) {
				t.Fatalf("ownership transfer error=%v", err)
			}
			if _, err := c.PolicyIdle(ctx, time.Now(), nil); err == nil {
				t.Fatal("idle policy displaced training")
			}
			after, err := s.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if after != before {
				t.Fatal("routine transition changed training state or fence")
			}
			if pending, err := s.PendingWork(ctx); err != nil || pending != 1 {
				t.Fatalf("pending=%d error=%v", pending, err)
			}
			if len(r.calls) != 0 || len(r.trainingCalls) != 0 {
				t.Fatalf("routine transition reached runtime: %v %v", r.calls, r.trainingCalls)
			}
			r.releaseErr = nil
			if _, err := c.RecoverTraining(ctx, job.ID); err != nil {
				t.Fatal(err)
			}
			if pending, err := s.PendingWork(ctx); err != nil || pending != 0 {
				t.Fatalf("released pending=%d error=%v", pending, err)
			}
			if _, err := c.Switch(ctx, control.WorkloadIdle, "operator"); err != nil {
				t.Fatalf("switch after verified release: %v", err)
			}
		})
	}
}

func TestTrainingContractIdentityCollisionPrecedesInferenceStop(t *testing.T) {
	for _, kind := range []string{"queued", "reserved", "registered"} {
		t.Run(kind, func(t *testing.T) {
			_, s, r := trainingStartContract(t)
			ctx := context.Background()
			snap, err := s.Catalog(ctx)
			if err != nil {
				t.Fatal(err)
			}
			catalog := snap.Catalog.Clone()
			catalog.Profiles = append(catalog.Profiles, control.WorkloadProfile{ID: control.WorkloadText, Label: "Text", Adapter: "systemd", Unit: "text.service", Cgroup: "/text", HealthURL: "http://localhost:8080/health"})
			if _, err = s.ReplaceCatalog(ctx, snap.Revision, catalog); err != nil {
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
			id := "existing-request"
			if kind == "registered" {
				if _, err := s.AdmitWorkToken(ctx, id, "", control.WorkloadText, state.LeaseFence); err != nil {
					t.Fatal(err)
				}
			} else {
				d := store.Demand{RequestID: id, Requester: "test", Workflow: "inference", BodySHA256: strings.Repeat("d", 64), Workload: control.WorkloadText}
				if err := s.QueueDemand(ctx, d); err != nil {
					t.Fatal(err)
				}
				if kind == "reserved" {
					if _, err := s.ReserveDemand(ctx, d); err != nil {
						t.Fatal(err)
					}
				}
			}
			before, err := s.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			r.active = control.WorkloadText
			c := testController(t, s, r)
			if _, err := c.StartTraining(ctx, id, "trainer"); !errors.Is(err, store.ErrRequestConflict) {
				t.Fatalf("collision: %v", err)
			}
			after, err := s.State(ctx)
			if err != nil || before != after {
				t.Fatalf("rejected request changed inference authority: %v", err)
			}
			if len(r.calls) != 0 || len(r.trainingCalls) != 0 {
				t.Fatalf("rejected request reached runtime: %v %v", r.calls, r.trainingCalls)
			}
		})
	}
}

func TestTrainingContractOutputCollisionPrecedesInferenceStop(t *testing.T) {
	for _, suffix := range []string{"", "-wal"} {
		t.Run("state"+suffix, func(t *testing.T) {
			_, template, _ := trainingStartContract(t)
			ctx := context.Background()
			snap, err := template.Catalog(ctx)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "state.db")
			s := openStoreAt(t, path)
			catalog := snap.Catalog.Clone()
			catalog.Profiles[0].Trainer.JobFile = path + suffix
			catalog.Profiles = append(catalog.Profiles, control.WorkloadProfile{ID: control.WorkloadText, Label: "Text", Adapter: "systemd", Unit: "text.service", Cgroup: "/text", HealthURL: "http://localhost:8080/health"})
			if _, err := s.ReplaceCatalog(ctx, "", catalog); err != nil {
				t.Fatal(err)
			}
			state, err := s.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			state.Owner = control.OwnerSupervisor
			state.ActiveWorkload = control.WorkloadText
			state.DesiredWorkload = control.WorkloadText
			state.Phase = control.PhaseStable
			state.Health = control.HealthHealthy
			state.Admission = control.AdmissionOpen
			before, err := s.UpdateState(ctx, state.Version, state)
			if err != nil {
				t.Fatal(err)
			}
			r := &trainingContractRuntime{fakeRuntime: fakeRuntime{active: control.WorkloadText}}
			c := testController(t, s, r)
			if _, err := c.StartTraining(ctx, "new-training", "trainer"); err == nil {
				t.Fatal("output collision accepted")
			}
			after, err := s.State(ctx)
			if err != nil || before != after {
				t.Fatalf("invalid output changed authority: %v", err)
			}
			if len(r.calls) != 0 || len(r.trainingCalls) != 0 {
				t.Fatalf("invalid output reached runtime: %v %v", r.calls, r.trainingCalls)
			}
		})
	}
}

func reserveTrainingFixture(ctx context.Context, s *store.Store, id, revision string) (control.TrainingJob, error) {
	claim, err := s.ClaimTraining(ctx, id, "trainer", revision)
	if err != nil {
		return claim, err
	}
	return s.ReserveTraining(ctx, claim)
}

func TestTrainingContractRecoverPreparationDoesNotStopInference(t *testing.T) {
	_, template, _ := trainingStartContract(t)
	ctx := context.Background()
	snap, err := template.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "state.db")
	s := openStoreAt(t, path)
	catalog := snap.Catalog.Clone()
	catalog.Profiles = append(catalog.Profiles, control.WorkloadProfile{ID: control.WorkloadText, Label: "Text", Adapter: "systemd", Unit: "text.service", Cgroup: "/text", HealthURL: "http://localhost:8080/health"})
	snap, err = s.ReplaceCatalog(ctx, "", catalog)
	if err != nil {
		t.Fatal(err)
	}
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state.Owner = control.OwnerSupervisor
	state.ActiveWorkload = control.WorkloadText
	state.DesiredWorkload = control.WorkloadText
	state.Phase = control.PhaseStable
	state.Health = control.HealthHealthy
	state.Admission = control.AdmissionOpen
	state, err = s.UpdateState(ctx, state.Version, state)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := s.ClaimTraining(ctx, "prepared-job", "trainer", snap.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStoreAt(t, path)
	r := &trainingContractRuntime{fakeRuntime: fakeRuntime{active: control.WorkloadText}}
	c := testController(t, reopened, r)
	status, err := c.TrainingStatus(ctx, claim.ID)
	if err != nil || status.Status != "preparing" {
		t.Fatalf("preparation not visible: %v", err)
	}
	recovered, err := c.RecoverTraining(ctx, claim.ID)
	if err != nil || recovered != state {
		t.Fatalf("preparation recovery changed inference: %v", err)
	}
	status, err = c.TrainingStatus(ctx, claim.ID)
	if err != nil || status.Status != "interrupted" {
		t.Fatalf("preparation not retired: %v", err)
	}
	if _, err := c.StartTraining(ctx, claim.ID, "trainer"); !errors.Is(err, store.ErrRequestConflict) {
		t.Fatalf("preparation replay: %v", err)
	}
	if len(r.calls) != 0 || len(r.trainingCalls) != 0 {
		t.Fatalf("recovery reached runtime: %v %v", r.calls, r.trainingCalls)
	}
}

type trainingReservationFailureStore struct {
	*store.Store
	commit   bool
	abortErr error
}

func (s *trainingReservationFailureStore) ReserveTraining(ctx context.Context, claim control.TrainingJob) (control.TrainingJob, error) {
	if s.commit {
		if _, err := s.Store.ReserveTraining(ctx, claim); err != nil {
			return control.TrainingJob{}, err
		}
	}
	return control.TrainingJob{}, errors.New("reservation acknowledgement unavailable")
}
func (s *trainingReservationFailureStore) AbortTrainingPreparation(ctx context.Context, claim control.TrainingJob) (control.State, error) {
	if s.abortErr != nil {
		return control.State{}, s.abortErr
	}
	return s.Store.AbortTrainingPreparation(ctx, claim)
}
func TestTrainingContractPreparationFailureRemainsRecoverable(t *testing.T) {
	for _, kind := range []string{"reservation-failed", "reservation-ambiguous", "abort-failed"} {
		t.Run(kind, func(t *testing.T) {
			_, s, r := trainingStartContract(t)
			wrapped := &trainingReservationFailureStore{Store: s, commit: kind == "reservation-ambiguous"}
			if kind == "abort-failed" {
				wrapped.abortErr = errors.New("database unavailable")
			}
			c := testController(t, wrapped, r)
			ctx := context.Background()
			if _, err := c.StartTraining(ctx, "preparation-failure", "trainer"); err == nil {
				t.Fatal("reservation uncertainty accepted")
			}
			job, err := s.TrainingJob(ctx, "preparation-failure")
			if err != nil {
				t.Fatal(err)
			}
			expected := map[string]string{"reservation-failed": "interrupted", "reservation-ambiguous": "launching", "abort-failed": "preparing"}[kind]
			if job.Status != expected {
				t.Fatalf("status=%s want=%s", job.Status, expected)
			}
			if len(r.trainingCalls) != 0 {
				t.Fatalf("failed reservation launched: %v", r.trainingCalls)
			}
			if kind == "reservation-failed" {
				return
			}
			fresh := testController(t, s, r)
			if _, err := fresh.RecoverTraining(ctx, job.ID); err != nil {
				t.Fatal(err)
			}
			job, err = s.TrainingJob(ctx, job.ID)
			if err != nil || job.Status != "interrupted" {
				t.Fatalf("retry recovery failed: %v", err)
			}
		})
	}
}

// Exercise the real read-only destination qualification while retaining the
// controller fixture's observable inference and launch effects.
type trainingOutputPreflightRuntime struct {
	*trainingContractRuntime
	manager *gpuruntime.SystemdManager
}

func (r *trainingOutputPreflightRuntime) Preflight(ctx context.Context) error {
	return r.manager.Preflight(ctx)
}

func TestTrainingContractInvalidDestinationsPrecedeInferenceHandoff(t *testing.T) {
	for _, kind := range []string{"missing job parent", "missing terminal parent", "unsafe job parent", "unsafe terminal parent", "linked job parent", "linked terminal parent", "job directory", "terminal directory", "unwritable job parent", "unwritable terminal parent", "unsearchable job parent", "unsearchable terminal parent", "root-owned job parent", "root-owned terminal parent"} {
		t.Run(kind, func(t *testing.T) {
			permissionCase := strings.HasPrefix(kind, "unwritable") || strings.HasPrefix(kind, "unsearchable") || strings.HasPrefix(kind, "root-owned")
			if permissionCase && os.Geteuid() == 0 {
				t.Skip("requires nonroot credentials to exercise denied output creation")
			}
			_, s, r := trainingStartContract(t)
			ctx := context.Background()
			snap, err := s.Catalog(ctx)
			if err != nil {
				t.Fatal(err)
			}
			catalog := snap.Catalog.Clone()
			binding := catalog.Profiles[0].Trainer
			dir := t.TempDir()
			binding.JobFile = filepath.Join(dir, "job.json")
			binding.TerminalFile = filepath.Join(dir, "terminal.json")
			output := &binding.JobFile
			if strings.Contains(kind, "terminal") {
				output = &binding.TerminalFile
			}
			switch {
			case strings.HasPrefix(kind, "root-owned"):
				// The filesystem root is trusted and searchable but cannot be
				// written by the supervisor's ordinary user credentials.
				*output = filepath.Join("/", filepath.Base(dir)+"-training-output.json")
			case strings.HasPrefix(kind, "unwritable"), strings.HasPrefix(kind, "unsearchable"):
				parent := filepath.Join(dir, "restricted")
				if err := os.Mkdir(parent, 0700); err != nil {
					t.Fatal(err)
				}
				// Restore traversal so TempDir cleanup works for the nonroot runner.
				t.Cleanup(func() { _ = os.Chmod(parent, 0700) })
				mode := os.FileMode(0500)
				if strings.HasPrefix(kind, "unsearchable") {
					mode = 0600
				}
				if err := os.Chmod(parent, mode); err != nil {
					t.Fatal(err)
				}
				*output = filepath.Join(parent, "output.json")
			case strings.HasPrefix(kind, "missing"):
				*output = filepath.Join(dir, "missing", "output.json")
			case strings.HasPrefix(kind, "unsafe"):
				parent := filepath.Join(dir, "unsafe")
				if err := os.Mkdir(parent, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(parent, 0777); err != nil {
					t.Fatal(err)
				}
				*output = filepath.Join(parent, "output.json")
			case strings.HasPrefix(kind, "linked"):
				parent := filepath.Join(dir, "link")
				if err := os.Symlink(dir, parent); err != nil {
					t.Fatal(err)
				}
				*output = filepath.Join(parent, "output.json")
			default:
				if err := os.Mkdir(*output, 0700); err != nil {
					t.Fatal(err)
				}
			}
			catalog.Profiles = append(catalog.Profiles, control.WorkloadProfile{ID: control.WorkloadText, Label: "Text", Adapter: "systemd", Unit: "text.service", Cgroup: "/text", HealthURL: "http://localhost:8080/health"})
			if _, err := s.ReplaceCatalog(ctx, snap.Revision, catalog); err != nil {
				t.Fatal(err)
			}
			state, err := s.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			state.ActiveWorkload = control.WorkloadText
			state.DesiredWorkload = control.WorkloadText
			state.Admission = control.AdmissionOpen
			before, err := s.UpdateState(ctx, state.Version, state)
			if err != nil {
				t.Fatal(err)
			}
			manager, err := gpuruntime.NewSystemdManager(gpuruntime.SystemdConfig{Catalog: &catalog, HealthTimeout: time.Second, SystemctlPath: "/usr/bin/true", NvidiaSMIPath: "/usr/bin/true"})
			if err != nil {
				t.Fatal(err)
			}
			r.active = control.WorkloadText
			c := testController(t, s, &trainingOutputPreflightRuntime{r, manager})
			if _, err := c.StartTraining(ctx, "invalid-output", "trainer"); !errors.Is(err, gpuruntime.ErrLaunchUnsupported) {
				t.Fatalf("invalid destination accepted or failed unrelated qualification: %v", err)
			}
			after, err := s.State(ctx)
			if err != nil || before != after || r.active != control.WorkloadText {
				t.Fatalf("invalid destination changed inference authority: %v", err)
			}
			if len(r.calls) != 0 || len(r.trainingCalls) != 0 {
				t.Fatalf("invalid destination caused runtime effects: %v %v", r.calls, r.trainingCalls)
			}
			if _, err := s.TrainingJob(ctx, "invalid-output"); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("invalid destination left a durable claim or job: %v", err)
			}
			if err := s.CheckTrainingRequestID(ctx, "invalid-output"); err != nil {
				t.Fatalf("invalid destination consumed request identity: %v", err)
			}
			if pending, err := s.PendingWork(ctx); err != nil || pending != 0 {
				t.Fatalf("invalid destination reserved admission: %d %v", pending, err)
			}
		})
	}
}
