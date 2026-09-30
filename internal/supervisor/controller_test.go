package supervisor

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

type fakeRuntime struct {
	active          control.Workload
	mediaReady      bool
	healthFailures  int
	startErr        error
	partialStart    bool
	stopErr         error
	releaseFailures int
	releaseCalls    int
	blockRelease    bool
	cancelOnStop    func()
	blockStop       bool
	calls           []string
}

func (r *fakeRuntime) Observe(context.Context) (gpuruntime.Snapshot, error) {
	return gpuruntime.Snapshot{
		TextActive: r.active == control.WorkloadText,
		MediaReady: r.mediaReady,
	}, nil
}

func (r *fakeRuntime) Start(_ context.Context, workload control.Workload) error {
	r.calls = append(r.calls, "start "+string(workload))
	if r.startErr != nil && workload == control.WorkloadMedia {
		if r.partialStart {
			r.active = control.WorkloadMedia
			r.mediaReady = true
		}
		return r.startErr
	}
	if workload == control.WorkloadText {
		r.active = control.WorkloadText
	} else {
		r.active = control.WorkloadMedia
		r.mediaReady = true
	}
	return nil
}

func (r *fakeRuntime) Stop(ctx context.Context, workload control.Workload) error {
	r.calls = append(r.calls, "stop "+string(workload))
	if r.blockStop {
		<-ctx.Done()
		return ctx.Err()
	}
	if r.cancelOnStop != nil {
		r.cancelOnStop()
	}
	if r.stopErr != nil {
		return r.stopErr
	}
	if r.active == workload {
		r.active = control.WorkloadIdle
	}
	return nil
}

func (r *fakeRuntime) StopForRecovery(ctx context.Context) error {
	for _, workload := range []control.Workload{control.WorkloadText, control.WorkloadMedia} {
		if err := r.Stop(ctx, workload); err != nil {
			return err
		}
	}
	r.mediaReady = false
	return nil
}

func (r *fakeRuntime) Released(ctx context.Context) error {
	r.releaseCalls++
	if r.blockRelease {
		<-ctx.Done()
		return ctx.Err()
	}
	if r.releaseFailures > 0 {
		r.releaseFailures--
		return errors.New("GPU memory not released")
	}
	return nil
}

func (r *fakeRuntime) Healthy(context.Context, control.Workload) error {
	if r.healthFailures > 0 {
		r.healthFailures--
		return errors.New("not ready")
	}
	return nil
}

func TestSwitchStopsTextBeforeStartingMedia(t *testing.T) {
	stateStore := openStore(t)
	runtime := &fakeRuntime{active: control.WorkloadText, mediaReady: true}
	controller := testController(t, stateStore, runtime)
	if _, err := controller.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := controller.Switch(context.Background(), control.WorkloadMedia, "test")
	if err != nil {
		t.Fatal(err)
	}
	if state.ActiveWorkload != control.WorkloadMedia || state.Admission != control.AdmissionOpen {
		t.Fatalf("state = %#v", state)
	}
	assertCalls(t, runtime.calls, "stop text", "start media")
	events, err := stateStore.TransitionEvents(context.Background(), "11111111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 ||
		events[0].Kind != "intent" || events[0].Action != "stop text" ||
		events[1].Kind != "observation" || events[1].Outcome != "ok" ||
		events[2].Kind != "intent" || events[2].Action != "start media" ||
		events[3].Kind != "observation" || events[3].Outcome != "ok" {
		t.Fatalf("events = %#v", events)
	}
}

func TestSwitchRequiresReconciliationBeforeAnyTarget(t *testing.T) {
	for _, target := range []control.Workload{control.WorkloadText, control.WorkloadMedia, control.WorkloadIdle} {
		t.Run(string(target), func(t *testing.T) {
			stateStore := openStore(t)
			runtime := &fakeRuntime{mediaReady: true}
			controller := testController(t, stateStore, runtime)
			before, err := stateStore.State(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if before.Phase != control.PhaseReconciling || before.Health != control.HealthHealthy {
				t.Fatalf("unexpected initial state = %#v", before)
			}
			state, err := controller.Switch(context.Background(), target, "test")
			if !errors.Is(err, ErrReconcileRequired) || state != before {
				t.Fatalf("unreconciled switch = %#v, error = %v", state, err)
			}
			after, err := stateStore.State(context.Background())
			if err != nil || after != before {
				t.Fatalf("unreconciled switch mutated state = %#v, error = %v", after, err)
			}
			if running, err := stateStore.InProgressTransition(context.Background()); err != nil || running != "" {
				t.Fatalf("unreconciled switch created transition = %q, error = %v", running, err)
			}
			if len(runtime.calls) != 0 {
				t.Fatalf("unreconciled switch changed runtime: %#v", runtime.calls)
			}

			reconciled, err := controller.Reconcile(context.Background())
			if err != nil || reconciled.Phase != control.PhaseStable {
				t.Fatalf("reconcile = %#v, error = %v", reconciled, err)
			}
			switched, err := controller.Switch(context.Background(), target, "test")
			if err != nil {
				t.Fatal(err)
			}
			admission := control.AdmissionOpen
			if target == control.WorkloadIdle {
				admission = control.AdmissionClosed
			}
			if switched.ActiveWorkload != target || switched.Phase != control.PhaseStable || switched.Admission != admission {
				t.Fatalf("reconciled switch = %#v", switched)
			}
		})
	}
}

func TestSwitchFailureRollsBackAndLatchesError(t *testing.T) {
	stateStore := openStore(t)
	runtime := &fakeRuntime{active: control.WorkloadText, mediaReady: true, startErr: errors.New("start failed")}
	controller := testController(t, stateStore, runtime)
	if _, err := controller.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := controller.Switch(context.Background(), control.WorkloadMedia, "test")
	if err == nil {
		t.Fatal("expected switch failure")
	}
	if state.ActiveWorkload != control.WorkloadText || state.Health != control.HealthError || state.Admission != control.AdmissionClosed {
		t.Fatalf("unsafe failed state = %#v", state)
	}
	assertCalls(t, runtime.calls, "stop text", "start media", "start text")
}

func TestFailedMediaStartRollsBackToIdleOnlyAfterRelease(t *testing.T) {
	for _, tc := range []struct {
		name         string
		blockRelease bool
		stopErr      error
		wantActive   control.Workload
	}{
		{"released", false, nil, control.WorkloadIdle},
		{"release failed", true, nil, control.WorkloadUnknown},
		{"media stop failed", false, errors.New("release endpoint failed"), control.WorkloadUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stateStore := openStore(t)
			runtime := &fakeRuntime{}
			controller := testController(t, stateStore, runtime)
			if _, err := controller.Reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			runtime.calls = nil
			runtime.releaseCalls = 0
			runtime.startErr = errors.New("partially started media")
			runtime.partialStart = true
			runtime.blockRelease = tc.blockRelease
			runtime.stopErr = tc.stopErr
			// Rollback includes real SQLite journal writes, so keep the normal cleanup
			// budget. Only the deliberately blocked release probe needs a short timeout.
			if tc.blockRelease {
				controller.config.ActionTimeout = time.Millisecond
			}
			state, err := controller.Switch(context.Background(), control.WorkloadMedia, "test")
			if !errors.Is(err, runtime.startErr) || state.ActiveWorkload != tc.wantActive ||
				state.Health != control.HealthError || state.Admission != control.AdmissionClosed {
				t.Fatalf("failed media start state = %#v, error = %v", state, err)
			}
			assertCalls(t, runtime.calls, "start media", "stop media")
			if tc.stopErr != nil && !errors.Is(err, tc.stopErr) {
				t.Fatalf("missing failed media stop: %v", err)
			}
			if tc.blockRelease && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("missing release probe timeout: %v", err)
			}
			if tc.stopErr == nil && runtime.releaseCalls == 0 {
				t.Fatal("rollback did not verify GPU release")
			}
			persisted, err := stateStore.State(context.Background())
			if err != nil || persisted != state {
				t.Fatalf("failed state was not persisted: %#v, error = %v", persisted, err)
			}
			if running, err := stateStore.InProgressTransition(context.Background()); err != nil || running != "" {
				t.Fatalf("failed transition remains in progress: %q, error = %v", running, err)
			}
		})
	}
}

func TestReadinessPollingAllowsDelayedHealth(t *testing.T) {
	stateStore := openStore(t)
	runtime := &fakeRuntime{active: control.WorkloadText, mediaReady: true}
	controller := testController(t, stateStore, runtime)
	if _, err := controller.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	runtime.healthFailures = 2
	state, err := controller.Switch(context.Background(), control.WorkloadMedia, "test")
	if err != nil {
		t.Fatal(err)
	}
	if state.ActiveWorkload != control.WorkloadMedia {
		t.Fatalf("state = %#v", state)
	}
}

func TestInterruptedTransitionRequiresExplicitRecovery(t *testing.T) {
	stateStore := openStore(t)
	runtime := &fakeRuntime{active: control.WorkloadText, mediaReady: true}
	controller := testController(t, stateStore, runtime)
	current, err := controller.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	target := current
	target.DesiredWorkload = control.WorkloadMedia
	_, err = stateStore.StartTransition(context.Background(), current.Version, store.Transition{
		ID: "interrupted", Source: current, Target: target, Previous: current,
		Initiator: "test", Phase: control.PhaseDraining, Deadline: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Reconcile(context.Background()); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("reconcile error = %v", err)
	}
	if running, err := stateStore.InProgressTransition(context.Background()); err != nil || running != "" {
		t.Fatalf("running transition = %q, error = %v", running, err)
	}
	if _, err := controller.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Switch(context.Background(), control.WorkloadMedia, "test"); err != nil {
		t.Fatal(err)
	}
}

func TestUserOwnershipBlocksSupervisorSwitchAndRecovery(t *testing.T) {
	stateStore := openStore(t)
	runtime := &fakeRuntime{mediaReady: true}
	controller := testController(t, stateStore, runtime)
	state, err := stateStore.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state.Owner = control.OwnerUser
	state.Phase = control.PhaseStable
	if _, err := stateStore.UpdateState(context.Background(), state.Version, state); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Switch(context.Background(), control.WorkloadText, "test"); !errors.Is(err, ErrUserOwned) {
		t.Fatalf("switch error = %v", err)
	}
	if _, err := controller.Recover(context.Background()); !errors.Is(err, ErrUserOwned) {
		t.Fatalf("recover error = %v", err)
	}
	if len(runtime.calls) != 0 {
		t.Fatalf("runtime calls = %#v", runtime.calls)
	}
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	stateStore, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stateStore.Close() })
	return stateStore
}

func testController(t *testing.T, stateStore StateStore, runtime gpuruntime.Manager) *Controller {
	t.Helper()
	controller, err := newController(stateStore, runtime, Config{
		DrainTimeout: time.Second, VerifyTimeout: time.Second,
		ActionTimeout:  time.Second,
		CleanupTimeout: time.Second, FinalizeTimeout: time.Second,
		PollInterval: time.Millisecond,
	}, time.Now, func() (string, error) {
		return "11111111-1111-4111-8111-111111111111", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return controller
}

func assertCalls(t *testing.T, actual []string, expected ...string) {
	t.Helper()
	if len(actual) != len(expected) {
		t.Fatalf("calls = %#v", actual)
	}
	for index := range expected {
		if actual[index] != expected[index] {
			t.Fatalf("calls = %#v", actual)
		}
	}
}

func TestSwitchToIdleReleasesMediaWithoutStoppingUI(t *testing.T) {
	stateStore := openStore(t)
	runtime := &fakeRuntime{active: control.WorkloadMedia, mediaReady: true}
	controller := testController(t, stateStore, runtime)
	state, err := stateStore.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state.Owner = control.OwnerSupervisor
	state.DesiredWorkload = control.WorkloadMedia
	state.ActiveWorkload = control.WorkloadMedia
	state.Phase = control.PhaseStable
	state.Admission = control.AdmissionOpen
	state, err = stateStore.UpdateState(context.Background(), state.Version, state)
	if err != nil {
		t.Fatal(err)
	}
	result, err := controller.Switch(context.Background(), control.WorkloadIdle, "test")
	if err != nil {
		t.Fatal(err)
	}
	if result.ActiveWorkload != control.WorkloadIdle || result.Admission != control.AdmissionClosed {
		t.Fatalf("state = %#v", result)
	}
	assertCalls(t, runtime.calls, "stop media")
}

func TestSwitchWaitsForGPUReleaseBeforeStartingText(t *testing.T) {
	stateStore := openStore(t)
	runtime := &fakeRuntime{active: control.WorkloadMedia, mediaReady: true, releaseFailures: 2}
	controller := testController(t, stateStore, runtime)
	state, err := stateStore.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state.ActiveWorkload = control.WorkloadMedia
	state.DesiredWorkload = control.WorkloadMedia
	state.Phase = control.PhaseStable
	state.Health = control.HealthHealthy
	state.Admission = control.AdmissionOpen
	if _, err := stateStore.UpdateState(context.Background(), state.Version, state); err != nil {
		t.Fatal(err)
	}
	result, err := controller.Switch(context.Background(), control.WorkloadText, "test")
	if err != nil {
		t.Fatal(err)
	}
	if result.ActiveWorkload != control.WorkloadText {
		t.Fatalf("active workload = %q", result.ActiveWorkload)
	}
	assertCalls(t, runtime.calls, "stop media", "start text")
}

func TestCanceledSwitchClosesTransitionAfterRollbackTimeout(t *testing.T) {
	stateStore := openStore(t)
	runtime := &fakeRuntime{active: control.WorkloadText, mediaReady: true}
	controller := testController(t, stateStore, runtime)
	if _, err := controller.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runtime.cancelOnStop = cancel
	if _, err := controller.Switch(ctx, control.WorkloadMedia, "test"); err == nil {
		t.Fatal("expected canceled switch")
	}
	running, err := stateStore.InProgressTransition(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if running != "" {
		t.Fatalf("transition remains in progress: %s", running)
	}
}

func TestRuntimeActionTimeoutClosesTransition(t *testing.T) {
	stateStore := openStore(t)
	runtime := &fakeRuntime{active: control.WorkloadText, mediaReady: true, blockStop: true}
	controller := testController(t, stateStore, runtime)
	controller.config.ActionTimeout = time.Millisecond
	if _, err := controller.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := controller.Switch(context.Background(), control.WorkloadMedia, "test")
	if err == nil {
		t.Fatal("expected action timeout")
	}
	if state.Health != control.HealthError || state.Admission != control.AdmissionClosed {
		t.Fatalf("unsafe failed state = %#v", state)
	}
	running, err := stateStore.InProgressTransition(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if running != "" {
		t.Fatalf("transition remains in progress: %s", running)
	}
}

func TestReleaseProbeTimeoutClosesTransition(t *testing.T) {
	stateStore := openStore(t)
	runtime := &fakeRuntime{active: control.WorkloadMedia, mediaReady: true, blockRelease: true}
	controller := testController(t, stateStore, runtime)
	controller.config.ActionTimeout = time.Millisecond
	state, err := stateStore.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state.ActiveWorkload = control.WorkloadMedia
	state.DesiredWorkload = control.WorkloadMedia
	state.Phase = control.PhaseStable
	state.Health = control.HealthHealthy
	state.Admission = control.AdmissionOpen
	if _, err := stateStore.UpdateState(context.Background(), state.Version, state); err != nil {
		t.Fatal(err)
	}
	result, err := controller.Switch(context.Background(), control.WorkloadText, "test")
	if err == nil {
		t.Fatal("expected release probe timeout")
	}
	if result.Health != control.HealthError || result.Admission != control.AdmissionClosed {
		t.Fatalf("unsafe failed state = %#v", result)
	}
	running, err := stateStore.InProgressTransition(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if running != "" {
		t.Fatalf("transition remains in progress: %s", running)
	}
}

func TestSwitchFromIdleReleasesOutOfBandMediaBeforeText(t *testing.T) {
	stateStore := openStore(t)
	runtime := &fakeRuntime{active: control.WorkloadIdle, mediaReady: true}
	controller := testController(t, stateStore, runtime)
	state, err := stateStore.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state.DesiredWorkload = control.WorkloadIdle
	state.ActiveWorkload = control.WorkloadIdle
	state.Phase = control.PhaseStable
	state.Health = control.HealthHealthy
	state.Admission = control.AdmissionClosed
	if _, err := stateStore.UpdateState(context.Background(), state.Version, state); err != nil {
		t.Fatal(err)
	}
	result, err := controller.Switch(context.Background(), control.WorkloadText, "test")
	if err != nil {
		t.Fatal(err)
	}
	if result.ActiveWorkload != control.WorkloadText {
		t.Fatalf("active workload = %q", result.ActiveWorkload)
	}
	assertCalls(t, runtime.calls, "stop media", "start text")
}

func TestStatusDoesNotMutateUserOwnedState(t *testing.T) {
	stateStore := openStore(t)
	runtime := &fakeRuntime{active: control.WorkloadText, mediaReady: true}
	controller := testController(t, stateStore, runtime)
	state, err := stateStore.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state.Owner = control.OwnerUser
	state.ActiveWorkload = control.WorkloadMedia
	state.DesiredWorkload = control.WorkloadMedia
	state.Phase = control.PhaseStable
	state.Health = control.HealthHealthy
	state.Admission = control.AdmissionOpen
	state, err = stateStore.UpdateState(context.Background(), state.Version, state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Status(context.Background()); err == nil {
		t.Fatal("expected invariant error")
	}
	after, err := stateStore.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if after != state {
		t.Fatalf("user-owned state changed: %#v != %#v", after, state)
	}
}

func TestSwitchIsIdempotent(t *testing.T) {
	tests := []struct {
		name      string
		workload  control.Workload
		admission control.Admission
	}{
		{name: "text", workload: control.WorkloadText, admission: control.AdmissionOpen},
		{name: "media", workload: control.WorkloadMedia, admission: control.AdmissionOpen},
		{name: "idle", workload: control.WorkloadIdle, admission: control.AdmissionClosed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stateStore := openStore(t)
			runtime := &fakeRuntime{active: test.workload, mediaReady: true, blockRelease: true}
			controller := testController(t, stateStore, runtime)
			state, err := stateStore.State(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			state.DesiredWorkload = test.workload
			state.ActiveWorkload = test.workload
			state.Phase = control.PhaseStable
			state.Health = control.HealthHealthy
			state.Admission = test.admission
			if _, err := stateStore.UpdateState(context.Background(), state.Version, state); err != nil {
				t.Fatal(err)
			}
			result, err := controller.Switch(context.Background(), test.workload, "test")
			if err != nil {
				t.Fatal(err)
			}
			if result.ActiveWorkload != test.workload || result.Health != control.HealthHealthy || result.Admission != test.admission {
				t.Fatalf("state = %#v", result)
			}
			if len(runtime.calls) != 0 {
				t.Fatalf("unexpected runtime calls: %#v", runtime.calls)
			}
		})
	}
}
