package supervisor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

type observationFailure struct{ *fakeRuntime }

func (observationFailure) Observe(context.Context) (gpuruntime.Snapshot, error) {
	return gpuruntime.Snapshot{}, errors.New("runtime unavailable")
}

type failingStore struct {
	StateStore
	stateErr      error
	transitionErr error
	startErr      error
	phaseErr      error
	pendingErr    error
}

func (s failingStore) State(ctx context.Context) (control.State, error) {
	if s.stateErr != nil {
		return control.State{}, s.stateErr
	}
	return s.StateStore.State(ctx)
}

func (s failingStore) InProgressTransition(ctx context.Context) (string, error) {
	if s.transitionErr != nil {
		return "", s.transitionErr
	}
	return s.StateStore.InProgressTransition(ctx)
}

func (s failingStore) StartTransition(ctx context.Context, version uint64, tr store.Transition) (control.State, error) {
	if s.startErr != nil {
		return control.State{}, s.startErr
	}
	return s.StateStore.StartTransition(ctx, version, tr)
}

func (s failingStore) SetTransitionPhase(ctx context.Context, id string, version uint64, phase control.Phase) (control.State, error) {
	if s.phaseErr != nil {
		return control.State{}, s.phaseErr
	}
	return s.StateStore.SetTransitionPhase(ctx, id, version, phase)
}

func (s failingStore) PendingTransitionWork(ctx context.Context, id string) (int, error) {
	if s.pendingErr != nil {
		return 0, s.pendingErr
	}
	return s.StateStore.PendingTransitionWork(ctx, id)
}

func TestStatusObservationFailureLatchesClosedState(t *testing.T) {
	stateStore := openStore(t)
	controller := testController(t, stateStore, observationFailure{&fakeRuntime{}})
	state, err := controller.Status(context.Background())
	if err == nil || state.ActiveWorkload != control.WorkloadUnknown ||
		state.Health != control.HealthError || state.Admission != control.AdmissionClosed {
		t.Fatalf("unsafe status = %#v, error = %v", state, err)
	}
	persisted, err := stateStore.State(context.Background())
	if err != nil || persisted != state {
		t.Fatalf("latched state = %#v, error = %v", persisted, err)
	}
	if _, err := controller.Switch(context.Background(), control.WorkloadText, "test"); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("error state accepted a new switch: %v", err)
	}
	if _, err := controller.Reconcile(context.Background()); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("latched error was cleared by boot reconciliation: %v", err)
	}
}

func TestReconcileLatchesRuntimeFailures(t *testing.T) {
	for _, test := range []struct {
		name    string
		runtime *fakeRuntime
	}{
		{"media stop", &fakeRuntime{stopErr: errors.New("stop failed")}},
		{"memory release", &fakeRuntime{blockRelease: true}},
		{"text health", &fakeRuntime{active: control.WorkloadText, healthFailures: 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			stateStore := openStore(t)
			controller := testController(t, stateStore, test.runtime)
			controller.config.ActionTimeout = time.Millisecond
			controller.config.VerifyTimeout = time.Millisecond
			state, err := controller.Reconcile(context.Background())
			if err == nil || state.Health != control.HealthError ||
				state.ActiveWorkload != control.WorkloadUnknown || state.Admission != control.AdmissionClosed {
				t.Fatalf("unsafe reconciliation = %#v, error = %v", state, err)
			}
		})
	}
}

func TestRecoveryVerifiesRuntimeBeforeOpeningAdmission(t *testing.T) {
	for _, test := range []struct {
		name      string
		runtime   *fakeRuntime
		wantError bool
		want      control.Workload
	}{
		{"idle", &fakeRuntime{}, false, control.WorkloadIdle},
		{"text", &fakeRuntime{active: control.WorkloadText}, false, control.WorkloadText},
		{"unhealthy text", &fakeRuntime{active: control.WorkloadText, healthFailures: 1}, true, control.WorkloadUnknown},
		{"failed media release", &fakeRuntime{stopErr: errors.New("release failed")}, true, control.WorkloadUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			stateStore := openStore(t)
			controller := testController(t, stateStore, test.runtime)
			state, err := controller.Recover(context.Background())
			if (err != nil) != test.wantError {
				t.Fatalf("recovery state = %#v, error = %v", state, err)
			}
			if test.wantError {
				if state.Admission != control.AdmissionClosed {
					t.Fatalf("admission opened after failed recovery: %#v", state)
				}
			} else if state.ActiveWorkload != test.want ||
				(state.Admission == control.AdmissionOpen) != (test.want == control.WorkloadText) {
				t.Fatalf("unexpected recovered state: %#v", state)
			}
		})
	}
}

func TestDrainDeadlineAndCancellationKeepWorkClosed(t *testing.T) {
	stateStore := openStore(t)
	runtime := &fakeRuntime{active: control.WorkloadText}
	controller := testController(t, stateStore, runtime)
	state, err := controller.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := stateStore.RegisterWork(context.Background(), "request", "job", control.WorkloadText, state.LeaseFence); err != nil {
		t.Fatal(err)
	}
	target := state
	target.DesiredWorkload = control.WorkloadMedia
	_, err = stateStore.StartTransition(context.Background(), state.Version, store.Transition{
		ID: "draining", Source: state, Target: target, Previous: state,
		Deadline: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.waitForDrain(context.Background(), "draining", time.Now().Add(-time.Second)); !errors.Is(err, ErrDrainTimeout) {
		t.Fatalf("pending work did not block transition: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := controller.waitForDrain(ctx, "draining", time.Now().Add(time.Minute)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled drain continued: %v", err)
	}
}

func TestReadinessRequiresObservedExclusiveOwner(t *testing.T) {
	stateStore := openStore(t)
	runtime := &fakeRuntime{active: control.WorkloadText, mediaReady: true}
	controller := testController(t, stateStore, runtime)
	if err := controller.waitReady(context.Background(), control.WorkloadMedia, time.Now().Add(-time.Second)); !errors.Is(err, ErrVerifyTimeout) ||
		!errors.Is(err, ErrStateVerification) {
		t.Fatalf("text owner accepted as media ready: %v", err)
	}
	runtime.active = control.WorkloadIdle
	runtime.mediaReady = false
	if err := controller.waitReady(context.Background(), control.WorkloadMedia, time.Now().Add(-time.Second)); !errors.Is(err, ErrStateVerification) {
		t.Fatalf("unready media accepted: %v", err)
	}
	runtime.active = control.WorkloadText
	if err := controller.waitReady(context.Background(), control.WorkloadIdle, time.Now().Add(-time.Second)); !errors.Is(err, ErrStateVerification) {
		t.Fatalf("active text accepted as idle: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := controller.waitReady(ctx, control.WorkloadIdle, time.Now().Add(time.Minute)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled readiness check continued: %v", err)
	}
}

func TestRollbackRestoresPreviousOwnerOrStopsUnexpectedText(t *testing.T) {
	for _, test := range []struct {
		name     string
		previous control.Workload
		active   control.Workload
		ready    bool
		calls    []string
	}{
		{"media from text", control.WorkloadMedia, control.WorkloadText, false, []string{"stop text", "start media"}},
		{"text from idle", control.WorkloadText, control.WorkloadIdle, false, []string{"start text"}},
		{"idle from text", control.WorkloadIdle, control.WorkloadText, false, []string{"stop text", "stop media"}},
		{"idle from media", control.WorkloadIdle, control.WorkloadMedia, true, []string{"stop media"}},
		{"media already ready", control.WorkloadMedia, control.WorkloadMedia, true, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			stateStore := openStore(t)
			runtime := &fakeRuntime{active: test.active, mediaReady: test.ready}
			controller := testController(t, stateStore, runtime)
			state, err := stateStore.State(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			_, err = stateStore.StartTransition(context.Background(), state.Version, store.Transition{
				ID: "rollback", Source: state, Target: state, Previous: state,
				Deadline: time.Now().Add(time.Minute),
			})
			if err != nil {
				t.Fatal(err)
			}
			previous := state
			previous.ActiveWorkload = test.previous
			if err := controller.rollback(context.Background(), "rollback", previous); err != nil {
				t.Fatal(err)
			}
			assertCalls(t, runtime.calls, test.calls...)
		})
	}
}

func TestRollbackFailureDoesNotClaimRestoredOwner(t *testing.T) {
	stateStore := openStore(t)
	runtime := &fakeRuntime{active: control.WorkloadText, startErr: errors.New("media unavailable")}
	controller := testController(t, stateStore, runtime)
	state, err := stateStore.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = stateStore.StartTransition(context.Background(), state.Version, store.Transition{
		ID: "failed-rollback", Source: state, Target: state, Previous: state,
		Deadline: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	previous := state
	previous.ActiveWorkload = control.WorkloadMedia
	if err := controller.rollback(context.Background(), "failed-rollback", previous); err == nil {
		t.Fatal("rollback start failure was ignored")
	}
	assertCalls(t, runtime.calls, "stop text", "start media")
}

func TestSwitchRejectsInvalidTargetAndTransitionIdentity(t *testing.T) {
	stateStore := openStore(t)
	runtime := &fakeRuntime{active: control.WorkloadText}
	controller := testController(t, stateStore, runtime)
	if _, err := controller.Switch(context.Background(), "unknown", "test"); err == nil {
		t.Fatal("unknown workload accepted")
	}
	if _, err := controller.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	controller.id = func() (string, error) { return "", errors.New("identity unavailable") }
	if _, err := controller.Switch(context.Background(), control.WorkloadMedia, "test"); err == nil {
		t.Fatal("transition without identity accepted")
	}
	if len(runtime.calls) != 0 {
		t.Fatalf("invalid requests changed runtime: %#v", runtime.calls)
	}
}

func TestSwitchRejectsConcurrentTransitionBeforeRuntimeEffects(t *testing.T) {
	stateStore := openStore(t)
	runtime := &fakeRuntime{active: control.WorkloadText}
	controller := testController(t, stateStore, runtime)
	current, err := controller.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	target := current
	target.DesiredWorkload = control.WorkloadMedia
	if _, err := stateStore.StartTransition(context.Background(), current.Version, store.Transition{
		ID: "already-running", Source: current, Target: target, Previous: current,
		Deadline: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Switch(context.Background(), control.WorkloadMedia, "other"); !errors.Is(err, ErrTransitionRunning) {
		t.Fatalf("concurrent transition accepted: %v", err)
	}
	if len(runtime.calls) != 0 {
		t.Fatalf("concurrent request changed runtime: %#v", runtime.calls)
	}
}

func TestTimedOutDrainRollsBackWithoutAdmittingWork(t *testing.T) {
	stateStore := openStore(t)
	runtime := &fakeRuntime{active: control.WorkloadText}
	controller := testController(t, stateStore, runtime)
	current, err := controller.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := stateStore.RegisterWork(context.Background(), "inflight", "job", control.WorkloadText, current.LeaseFence); err != nil {
		t.Fatal(err)
	}
	controller.config.DrainTimeout = time.Nanosecond
	state, err := controller.Switch(context.Background(), control.WorkloadMedia, "test")
	if !errors.Is(err, ErrDrainTimeout) || state.Health != control.HealthError ||
		state.Admission != control.AdmissionClosed || state.ActiveWorkload != control.WorkloadText {
		t.Fatalf("unsafe timed-out drain = %#v, error = %v", state, err)
	}
	if id, err := stateStore.InProgressTransition(context.Background()); err != nil || id != "" {
		t.Fatalf("timed-out transition remained active: %q, %v", id, err)
	}
}

func TestStatusReportsObservedTextWithoutMutatingStore(t *testing.T) {
	stateStore := openStore(t)
	runtime := &fakeRuntime{active: control.WorkloadText}
	controller := testController(t, stateStore, runtime)
	before, err := controller.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	status, err := controller.Status(context.Background())
	if err != nil || status.ActiveWorkload != control.WorkloadText {
		t.Fatalf("status = %#v, error = %v", status, err)
	}
	after, err := stateStore.State(context.Background())
	if err != nil || after != before {
		t.Fatalf("status mutated persisted state: %#v, %v", after, err)
	}
}

func TestConstructorRejectsMissingStoreRuntimeAndDeadlines(t *testing.T) {
	stateStore := openStore(t)
	runtime := &fakeRuntime{}
	if _, err := New(nil, runtime, Config{}); err == nil {
		t.Fatal("nil store accepted")
	}
	if _, err := New(stateStore, nil, Config{}); err == nil {
		t.Fatal("nil runtime accepted")
	}
	if _, err := New(stateStore, runtime, Config{}); err == nil {
		t.Fatal("zero deadlines accepted")
	}
	if _, err := New(stateStore, runtime, Config{
		DrainTimeout: time.Second, VerifyTimeout: time.Second,
		ActionTimeout: time.Second, CleanupTimeout: time.Second,
		FinalizeTimeout: time.Second, PollInterval: time.Millisecond,
	}); err != nil {
		t.Fatalf("valid controller rejected: %v", err)
	}
}

func TestStoreReadFailurePreventsRuntimeEffects(t *testing.T) {
	stateStore := openStore(t)
	runtime := &fakeRuntime{active: control.WorkloadText}
	controller := testController(t, failingStore{StateStore: stateStore, stateErr: errors.New("disk unavailable")}, runtime)
	for name, operation := range map[string]func() error{
		"status": func() error { _, err := controller.Status(context.Background()); return err },
		"switch": func() error {
			_, err := controller.Switch(context.Background(), control.WorkloadMedia, "test")
			return err
		},
		"reconcile": func() error { _, err := controller.Reconcile(context.Background()); return err },
		"recover":   func() error { _, err := controller.Recover(context.Background()); return err },
	} {
		if err := operation(); err == nil {
			t.Fatalf("%s accepted unreadable state", name)
		}
	}
	if len(runtime.calls) != 0 {
		t.Fatalf("storage failure changed runtime: %#v", runtime.calls)
	}
}

func TestSwitchFailsClosedWhenTransitionJournalCannotProgress(t *testing.T) {
	for _, test := range []struct {
		name   string
		inject func(*failingStore)
	}{
		{"in-progress read", func(s *failingStore) { s.transitionErr = errors.New("journal read failed") }},
		{"transition insert", func(s *failingStore) { s.startErr = errors.New("journal insert failed") }},
		{"work snapshot read", func(s *failingStore) { s.pendingErr = errors.New("snapshot failed") }},
		{"phase update", func(s *failingStore) { s.phaseErr = errors.New("phase write failed") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			stateStore := openStore(t)
			runtime := &fakeRuntime{active: control.WorkloadText}
			base := testController(t, stateStore, runtime)
			if _, err := base.Reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			wrapped := &failingStore{StateStore: stateStore}
			test.inject(wrapped)
			controller := testController(t, wrapped, runtime)
			if _, err := controller.Switch(context.Background(), control.WorkloadMedia, "test"); err == nil {
				t.Fatal("failed journal allowed transition")
			}
			if runtime.active != control.WorkloadText || len(runtime.calls) != 0 {
				t.Fatalf("journal failure changed runtime: %#v", runtime.calls)
			}
		})
	}
}

func TestUserOwnedObservationFailureDoesNotLatchSupervisorState(t *testing.T) {
	stateStore := openStore(t)
	state, err := stateStore.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state.Owner = control.OwnerUser
	state.Phase = control.PhaseStable
	state, err = stateStore.UpdateState(context.Background(), state.Version, state)
	if err != nil {
		t.Fatal(err)
	}
	controller := testController(t, stateStore, observationFailure{&fakeRuntime{}})
	if observed, err := controller.Status(context.Background()); err == nil || observed != state {
		t.Fatalf("user state changed on observation failure: %#v, %v", observed, err)
	}
	if _, err := controller.Reconcile(context.Background()); !errors.Is(err, ErrUserOwned) {
		t.Fatalf("user-owned state reconciled: %v", err)
	}
	stored, err := stateStore.State(context.Background())
	if err != nil || stored != state {
		t.Fatalf("user state mutated: %#v, %v", stored, err)
	}
}

func TestRecoveryObservationFailureLeavesAdmissionClosed(t *testing.T) {
	stateStore := openStore(t)
	controller := testController(t, stateStore, observationFailure{&fakeRuntime{}})
	state, err := controller.Recover(context.Background())
	if err == nil || state.Admission != control.AdmissionClosed {
		t.Fatalf("unobserved recovery = %#v, %v", state, err)
	}
}

func TestFailureAuditClassifiesSafetyErrors(t *testing.T) {
	for _, test := range []struct {
		err  error
		code string
	}{
		{context.Canceled, "canceled"},
		{ErrDrainTimeout, "timeout"},
		{ErrRuntimeObservation, "runtime-observation"},
		{ErrInvariant, "state-mismatch"},
		{ErrHealthCheck, "health-check"},
		{errors.New("unexpected"), "failed"},
	} {
		if code := failureCode(test.err); code != test.code {
			t.Fatalf("%v classified as %q, want %q", test.err, code, test.code)
		}
	}
}
