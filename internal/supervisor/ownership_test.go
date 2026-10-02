package supervisor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

func ownershipState(t *testing.T, stateStore *store.Store, owner control.Owner, workload control.Workload) control.State {
	t.Helper()
	state, err := stateStore.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state.Owner, state.DesiredWorkload, state.ActiveWorkload = owner, workload, workload
	state.Phase, state.Health, state.Admission = control.PhaseStable, control.HealthHealthy, control.AdmissionClosed
	if owner == control.OwnerSupervisor && workload != control.WorkloadIdle {
		state.Admission = control.AdmissionOpen
	}
	state, err = stateStore.UpdateState(context.Background(), state.Version, state)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestOwnershipTransferMatrix(t *testing.T) {
	for _, source := range []control.Owner{control.OwnerSupervisor, control.OwnerUser} {
		for _, workload := range []control.Workload{control.WorkloadText, control.WorkloadMedia, control.WorkloadIdle} {
			for _, target := range []control.Workload{control.WorkloadText, control.WorkloadMedia, control.WorkloadIdle} {
				t.Run(string(source)+"/"+string(workload)+"/"+string(target), func(t *testing.T) {
					stateStore := openStore(t)
					before := ownershipState(t, stateStore, source, workload)
					runtime := &fakeRuntime{active: workload, mediaReady: true}
					controller := testController(t, stateStore, runtime)
					change := controller.TransferToUser
					wantOwner := control.OwnerUser
					if source == control.OwnerUser {
						change, wantOwner = controller.TransferToSupervisor, control.OwnerSupervisor
					}
					state, err := change(context.Background(), target, "operator")
					if err != nil {
						t.Fatal(err)
					}
					admission := control.AdmissionClosed
					if wantOwner == control.OwnerSupervisor && target != control.WorkloadIdle {
						admission = control.AdmissionOpen
					}
					if state.Owner != wantOwner || state.ActiveWorkload != target || state.DesiredWorkload != target || state.Phase != control.PhaseStable || state.Health != control.HealthHealthy || state.Admission != admission || state.LeaseFence.Epoch != before.LeaseFence.Epoch+1 {
						t.Fatalf("bad transfer: %#v", state)
					}
					if _, err := stateStore.AdmitWorkToken(context.Background(), "stale", "", target, before.LeaseFence); !errors.Is(err, store.ErrStaleFence) && target != control.WorkloadIdle {
						t.Fatalf("old fence accepted: %v", err)
					}
				})
			}
		}
	}
}

func TestUserSwitchesNeverOpenSupervisorAdmission(t *testing.T) {
	for _, target := range []control.Workload{control.WorkloadText, control.WorkloadMedia, control.WorkloadIdle} {
		t.Run(string(target), func(t *testing.T) {
			stateStore := openStore(t)
			before := ownershipState(t, stateStore, control.OwnerUser, control.WorkloadText)
			runtime := &fakeRuntime{active: control.WorkloadText, mediaReady: true}
			controller := testController(t, stateStore, runtime)
			state, err := controller.SwitchUser(context.Background(), target, "operator")
			if err != nil || state.Owner != control.OwnerUser || state.ActiveWorkload != target || state.Admission != control.AdmissionClosed || state.LeaseFence == before.LeaseFence {
				t.Fatalf("user switch %#v: %v", state, err)
			}
			for _, supervisorTarget := range []control.Workload{control.WorkloadText, control.WorkloadMedia, control.WorkloadIdle} {
				if _, err := controller.Switch(context.Background(), supervisorTarget, "automation"); !errors.Is(err, ErrUserOwned) {
					t.Fatalf("supervisor bypass: %v", err)
				}
			}
			if _, err := stateStore.AdmitWorkToken(context.Background(), "new", "", control.WorkloadText, state.LeaseFence); !errors.Is(err, store.ErrAdmissionClosed) {
				t.Fatalf("registered user work: %v", err)
			}
		})
	}
}

func TestOwnershipCommandsValidateTargetAndSourceBeforeMutation(t *testing.T) {
	stateStore := openStore(t)
	before := ownershipState(t, stateStore, control.OwnerSupervisor, control.WorkloadText)
	runtime := &fakeRuntime{active: control.WorkloadText}
	controller := testController(t, stateStore, runtime)
	commands := []func(context.Context, control.Workload, string) (control.State, error){controller.TransferToUser, controller.SwitchUser, controller.TransferToSupervisor, controller.RecoverUser}
	for _, command := range commands {
		for _, target := range []control.Workload{"", "auto", control.WorkloadUnknown} {
			if _, err := command(context.Background(), target, "test"); err == nil {
				t.Fatal("invalid target accepted")
			}
		}
	}
	for _, command := range commands[1:] {
		if _, err := command(context.Background(), control.WorkloadText, "test"); !errors.Is(err, ErrSupervisorOwned) {
			t.Fatalf("invalid source: %v", err)
		}
	}
	after, err := stateStore.State(context.Background())
	if err != nil || before != after || len(runtime.calls) > 0 {
		t.Fatalf("rejection mutated state: %#v %v", after, err)
	}
}

func TestTransferDrainsRegisteredWorkBeforeChangingRuntime(t *testing.T) {
	stateStore := openStore(t)
	before := ownershipState(t, stateStore, control.OwnerSupervisor, control.WorkloadText)
	token, err := stateStore.AdmitWorkToken(context.Background(), "job", "", control.WorkloadText, before.LeaseFence)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &fakeRuntime{active: control.WorkloadText, mediaReady: true}
	controller := testController(t, stateStore, runtime)
	type result struct {
		state control.State
		err   error
	}
	done := make(chan result, 1)
	go func() {
		state, err := controller.TransferToUser(context.Background(), control.WorkloadMedia, "test")
		done <- result{state, err}
	}()
	deadline := time.Now().Add(time.Second)
	for {
		state, err := stateStore.State(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if state.Phase == control.PhaseDraining {
			if state.Owner != control.OwnerSupervisor || state.Admission != control.AdmissionClosed {
				t.Fatalf("premature transfer: %#v", state)
			}
			if _, err := stateStore.AdmitWorkToken(context.Background(), "late", "", control.WorkloadText, state.LeaseFence); !errors.Is(err, store.ErrAdmissionClosed) {
				t.Fatalf("late work accepted: %v", err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("did not enter draining")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case got := <-done:
		t.Fatalf("finished before work: %#v", got)
	default:
	}
	if err := stateStore.FinishWorkToken(context.Background(), "job", control.WorkloadText, before.LeaseFence, token, store.WorkCompleted); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil || got.state.Owner != control.OwnerUser {
		t.Fatalf("drained transfer: %#v", got)
	}
	assertCalls(t, runtime.calls, "stop text", "start media")
}

func TestFailedUserChangesNeverRestartStoppedWork(t *testing.T) {
	for _, transfer := range []bool{false, true} {
		for _, failure := range []string{"start", "health", "cancel", "phase"} {
			t.Run(failure+map[bool]string{false: "/switch", true: "/return"}[transfer], func(t *testing.T) {
				stateStore := openStore(t)
				ownershipState(t, stateStore, control.OwnerUser, control.WorkloadText)
				runtime := &fakeRuntime{active: control.WorkloadText, mediaReady: true}
				controller := testController(t, stateStore, runtime)
				controller.config.VerifyTimeout = 5 * time.Millisecond
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				switch failure {
				case "start":
					runtime.startErr = errors.New("failed start")
				case "health":
					runtime.healthFailures = 1000
				case "cancel":
					runtime.cancelOnStop = cancel
				case "phase":
					controller.store = &phaseFailureAfterStop{StateStore: stateStore}
				}
				change := controller.SwitchUser
				if transfer {
					change = controller.TransferToSupervisor
				}
				state, err := change(ctx, control.WorkloadMedia, "operator")
				if err == nil || state.Owner != control.OwnerUser || state.Health != control.HealthError || state.Admission != control.AdmissionClosed || state.ActiveWorkload != control.WorkloadUnknown {
					t.Fatalf("failure: %#v %v", state, err)
				}
				for _, call := range runtime.calls {
					if call == "start text" {
						t.Fatalf("resurrected user work: %v", runtime.calls)
					}
				}
				if _, err := change(context.Background(), control.WorkloadText, "operator"); !errors.Is(err, ErrRecoveryRequired) {
					t.Fatalf("unrecovered change: %v", err)
				}
			})
		}
	}
}

type phaseFailureAfterStop struct{ StateStore }

func (s phaseFailureAfterStop) SetTransitionPhase(ctx context.Context, id string, version uint64, phase control.Phase) (control.State, error) {
	if phase == control.PhaseLoading {
		return control.State{}, errors.New("phase write failed")
	}
	return s.StateStore.SetTransitionPhase(ctx, id, version, phase)
}

func TestUserRecoveryVerifiesExplicitTargetWithoutRuntimeChanges(t *testing.T) {
	for _, target := range []control.Workload{control.WorkloadText, control.WorkloadMedia, control.WorkloadIdle} {
		t.Run(string(target), func(t *testing.T) {
			stateStore := openStore(t)
			before := ownershipState(t, stateStore, control.OwnerUser, control.WorkloadText)
			before.Health, before.Phase = control.HealthError, control.PhaseReconciling
			if _, err := stateStore.UpdateState(context.Background(), before.Version, before); err != nil {
				t.Fatal(err)
			}
			runtime := &fakeRuntime{active: target, mediaReady: target == control.WorkloadMedia}
			controller := testController(t, stateStore, runtime)
			state, err := controller.RecoverUser(context.Background(), target, "operator")
			if err != nil || state.Owner != control.OwnerUser || state.ActiveWorkload != target || state.Health != control.HealthHealthy || state.Admission != control.AdmissionClosed || len(runtime.calls) > 0 {
				t.Fatalf("recovery: %#v %v calls %v", state, err, runtime.calls)
			}
			if target == control.WorkloadIdle && runtime.releaseCalls == 0 {
				t.Fatal("idle recovery did not verify release")
			}
		})
	}
}

func TestUserRecoveryFailureLeavesClosedOwnership(t *testing.T) {
	stateStore := openStore(t)
	ownershipState(t, stateStore, control.OwnerUser, control.WorkloadText)
	runtime := &fakeRuntime{active: control.WorkloadIdle, blockRelease: true}
	controller := testController(t, stateStore, runtime)
	controller.config.VerifyTimeout = time.Millisecond
	controller.config.ActionTimeout = time.Millisecond
	state, err := controller.RecoverUser(context.Background(), control.WorkloadIdle, "operator")
	if err == nil || state.Owner != control.OwnerUser || state.Health != control.HealthError || state.Admission != control.AdmissionClosed || len(runtime.calls) > 0 {
		t.Fatalf("recovery failure: %#v %v", state, err)
	}
}

type crashStore struct {
	StateStore
	point string
}

func (s crashStore) StartTransition(ctx context.Context, version uint64, tr store.Transition) (control.State, error) {
	state, err := s.StateStore.StartTransition(ctx, version, tr)
	if err == nil && s.point == "draining" {
		panic("process stopped")
	}
	return state, err
}
func (s crashStore) SetTransitionPhase(ctx context.Context, id string, version uint64, phase control.Phase) (control.State, error) {
	state, err := s.StateStore.SetTransitionPhase(ctx, id, version, phase)
	if err == nil && s.point == string(phase) {
		panic("process stopped")
	}
	return state, err
}
func (s crashStore) FinishTransition(ctx context.Context, id, status string, version uint64, state control.State) (control.State, error) {
	if s.point == "commit" {
		return control.State{}, errors.New("commit unavailable")
	}
	final, err := s.StateStore.FinishTransition(ctx, id, status, version, state)
	if err == nil && s.point == "committed" {
		panic("process stopped")
	}
	return final, err
}

func TestOwnershipCrashBoundariesPreserveCommittedOwner(t *testing.T) {
	for _, source := range []control.Owner{control.OwnerSupervisor, control.OwnerUser} {
		for _, point := range []string{"draining", "unloading", "loading", "verifying", "commit", "committed"} {
			t.Run(string(source)+"/"+point, func(t *testing.T) {
				ctx := context.Background()
				path := filepath.Join(t.TempDir(), "state.db")
				stateStore, err := store.Open(ctx, path)
				if err != nil {
					t.Fatal(err)
				}
				before := ownershipState(t, stateStore, source, control.WorkloadText)
				runtime := &fakeRuntime{active: control.WorkloadText, mediaReady: true}
				controller := testController(t, crashStore{stateStore, point}, runtime)
				change := controller.TransferToUser
				targetOwner := control.OwnerUser
				if source == control.OwnerUser {
					change = controller.TransferToSupervisor
					targetOwner = control.OwnerSupervisor
				}
				func() {
					defer func() {
						if p := recover(); p != nil && p != "process stopped" {
							panic(p)
						}
					}()
					_, _ = change(ctx, control.WorkloadMedia, "operator")
				}()
				if err := stateStore.Close(); err != nil {
					t.Fatal(err)
				}
				reopened, err := store.Open(ctx, path)
				if err != nil {
					t.Fatal(err)
				}
				defer reopened.Close()
				stored, err := reopened.State(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if stored.LeaseFence.Epoch != before.LeaseFence.Epoch+1 {
					t.Fatalf("restart fence: %#v", stored)
				}
				wantOwner := source
				if point == "committed" {
					wantOwner = targetOwner
				}
				if stored.Owner != wantOwner {
					t.Fatalf("premature owner commit: %#v", stored)
				}
				if point != "committed" {
					runtime.calls = nil
					fresh := testController(t, reopened, runtime)
					state, err := fresh.Reconcile(ctx)
					if !errors.Is(err, ErrRecoveryRequired) || state.Owner != source || state.Admission != control.AdmissionClosed || len(runtime.calls) > 0 {
						t.Fatalf("restart recovery: %#v %v %v", state, err, runtime.calls)
					}
				}
				// Ownership and target are retained in the durable transition audit.
				db, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				var sourceJSON, targetJSON []byte
				var status, initiator string
				if err := db.QueryRow(`SELECT source_state,target_state,status,initiator FROM transitions`).Scan(&sourceJSON, &targetJSON, &status, &initiator); err != nil {
					t.Fatal(err)
				}
				var auditSource, auditTarget control.State
				if err := json.Unmarshal(sourceJSON, &auditSource); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(targetJSON, &auditTarget); err != nil {
					t.Fatal(err)
				}
				wantStatus := "failed"
				if point == "committed" {
					wantStatus = "committed"
				}
				if auditSource.Owner != source || auditTarget.Owner != targetOwner || auditTarget.DesiredWorkload != control.WorkloadMedia || status != wantStatus || initiator != "operator" {
					t.Fatalf("audit source=%#v target=%#v status=%s initiator=%s", auditSource, auditTarget, status, initiator)
				}
			})
		}
	}
}

func TestUserHandoffTimeoutStopsWorkWithoutStartingTarget(t *testing.T) {
	stateStore := openStore(t)
	ownershipState(t, stateStore, control.OwnerUser, control.WorkloadText)
	gate, err := stateStore.AcquireUserExecution(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	runtime := &fakeRuntime{active: control.WorkloadText, mediaReady: true}
	controller := testController(t, stateStore, runtime)
	controller.config.DrainTimeout = 10 * time.Millisecond
	state, err := controller.TransferToSupervisor(context.Background(), control.WorkloadText, "operator")
	if !errors.Is(err, context.DeadlineExceeded) || state.Owner != control.OwnerUser || state.Health != control.HealthError || state.Admission != control.AdmissionClosed {
		t.Fatalf("stalled handoff: %#v %v", state, err)
	}
	assertCalls(t, runtime.calls, "stop text", "stop media")
	if runtime.active != control.WorkloadIdle {
		t.Fatal("user work was restarted")
	}
}
