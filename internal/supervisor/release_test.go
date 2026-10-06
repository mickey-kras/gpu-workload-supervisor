package supervisor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

type releaseOrderedRuntime struct {
	fakeRuntime
	exclusive bool
	verified  bool
}

func (r *releaseOrderedRuntime) Observe(ctx context.Context) (gpuruntime.Snapshot, error) {
	s, err := r.fakeRuntime.Observe(ctx)
	media := s.Workloads[control.WorkloadMedia]
	media.Exclusive = r.exclusive
	s.Workloads[control.WorkloadMedia] = media
	return s, err
}

func (r *releaseOrderedRuntime) ReleasedFor(ctx context.Context, target control.Workload) error {
	if err := r.fakeRuntime.ReleasedFor(ctx, target); err != nil {
		return err
	}
	r.verified = true
	return nil
}

func (r *releaseOrderedRuntime) Start(ctx context.Context, workload control.Workload) error {
	if !r.verified {
		return errors.New("start attempted before release verification")
	}
	return r.fakeRuntime.Start(ctx, workload)
}

func TestTextStopWaitsForReleaseInBothPolicies(t *testing.T) {
	for _, exclusive := range []bool{false, true} {
		for _, target := range []control.Workload{control.WorkloadMedia, control.WorkloadIdle} {
			for _, delayed := range []bool{false, true} {
				stateStore := openStore(t)
				ownershipState(t, stateStore, control.OwnerSupervisor, control.WorkloadText)
				runtime := &releaseOrderedRuntime{fakeRuntime: fakeRuntime{active: control.WorkloadText}, exclusive: exclusive}
				runtime.releaseFailures = 1000000
				if delayed {
					runtime.releaseFailures = 2
				}
				controller := testController(t, stateStore, runtime)
				controller.config.VerifyTimeout = 20 * time.Millisecond
				controller.config.CleanupTimeout = 20 * time.Millisecond
				state, err := controller.Switch(context.Background(), target, "test")
				if delayed {
					if err != nil || state.ActiveWorkload != target || runtime.releaseCalls < 3 {
						t.Fatalf("exclusive=%v target=%s: %#v %v", exclusive, target, state, err)
					}
				} else {
					if !errors.Is(err, ErrVerifyTimeout) || state.Health != control.HealthError || state.Admission != control.AdmissionClosed {
						t.Fatalf("unverified release: %#v %v", state, err)
					}
					for _, call := range runtime.calls {
						if call == "start media" {
							t.Fatal("started target before release")
						}
					}
				}
			}
		}
	}
}

func TestFailedUserReleaseAndCapacityPreserveOwnerAndNeverRestart(t *testing.T) {
	for _, transfer := range []bool{false, true} {
		for _, capacity := range []bool{false, true} {
			stateStore := openStore(t)
			before := ownershipState(t, stateStore, control.OwnerUser, control.WorkloadText)
			runtime := &fakeRuntime{active: control.WorkloadText}
			if capacity {
				runtime.startErr = gpuruntime.ErrCapacity
			} else {
				runtime.releaseFailures = 1000000
			}
			controller := testController(t, stateStore, runtime)
			controller.id = control.NewUUID
			controller.config.VerifyTimeout = 5 * time.Millisecond
			change := controller.SwitchUser
			if transfer {
				change = controller.TransferToSupervisor
			}
			state, err := change(context.Background(), control.WorkloadMedia, "test")
			if err == nil || state.Owner != control.OwnerUser || state.Health != control.HealthError || state.Admission != control.AdmissionClosed || state.LeaseFence == before.LeaseFence {
				t.Fatalf("state=%#v err=%v", state, err)
			}
			if capacity && (!errors.Is(err, gpuruntime.ErrCapacity) || failureCode(err) != "capacity") {
				t.Fatalf("capacity mislabeled: %v", err)
			}
			for _, call := range runtime.calls {
				if call == "start text" || (!capacity && call == "start media") {
					t.Fatalf("unsafe start: %v", runtime.calls)
				}
			}
			if _, err := change(context.Background(), control.WorkloadIdle, "test"); !errors.Is(err, ErrRecoveryRequired) {
				t.Fatalf("failure not latched: %v", err)
			}
			runtime.releaseFailures = 0
			state, err = controller.RecoverUser(context.Background(), control.WorkloadIdle, "test")
			if err != nil || state.Owner != control.OwnerUser || state.ActiveWorkload != control.WorkloadIdle || state.Admission != control.AdmissionClosed {
				t.Fatalf("explicit idle recovery: %#v %v", state, err)
			}
		}
	}
}

// SystemdManager.Healthy checks opposing cgroups. Verify-only recovery must not
// bypass this contract even when the target's service is responding normally.
func TestVerifyOnlyRecoveryRejectsOpposingEvidenceWithoutStoppingTarget(t *testing.T) {
	for _, target := range []control.Workload{control.WorkloadText, control.WorkloadMedia} {
		stateStore := openStore(t)
		ownershipState(t, stateStore, control.OwnerUser, target)
		runtime := &fakeRuntime{active: target, mediaReady: target == control.WorkloadMedia, healthFailures: 1000000}
		controller := testController(t, stateStore, runtime)
		controller.config.VerifyTimeout = 5 * time.Millisecond
		state, err := controller.RecoverUser(context.Background(), target, "test")
		if !errors.Is(err, ErrHealthCheck) || state.Owner != control.OwnerUser || state.Health != control.HealthError || state.Admission != control.AdmissionClosed || len(runtime.calls) != 0 {
			t.Fatalf("target=%s state=%#v calls=%v err=%v", target, state, runtime.calls, err)
		}
	}
}

type targetReleaseRuntime struct {
	fakeRuntime
	target control.Workload
}

func (r *targetReleaseRuntime) ReleasedFor(ctx context.Context, target control.Workload) error {
	r.target = target
	return r.fakeRuntime.ReleasedFor(ctx, target)
}

func (r *targetReleaseRuntime) Start(ctx context.Context, target control.Workload) error {
	if r.target != target {
		return errors.New("wrong release target")
	}
	return r.fakeRuntime.Start(ctx, target)
}

func TestTextAndIdleToMediaUseTargetAwareReleaseWhenUIRemainsAlive(t *testing.T) {
	for _, source := range []control.Workload{control.WorkloadText, control.WorkloadIdle} {
		stateStore := openStore(t)
		ownershipState(t, stateStore, control.OwnerSupervisor, source)
		runtime := &targetReleaseRuntime{fakeRuntime: fakeRuntime{active: source, mediaReady: true, releaseFailures: 1}}
		controller := testController(t, stateStore, runtime)
		state, err := controller.Switch(context.Background(), control.WorkloadMedia, "test")
		if err != nil || state.ActiveWorkload != control.WorkloadMedia || runtime.releaseCalls < 2 {
			t.Fatalf("source=%s state=%#v err=%v", source, state, err)
		}
		assertCalls(t, runtime.calls, "stop text", "start media")
	}
}

type unsupportedReleaseRuntime struct {
	fakeRuntime
	probes int
}

func (r *unsupportedReleaseRuntime) ReleasedFor(context.Context, control.Workload) error {
	r.probes++
	return gpuruntime.ErrUnloadUnverified
}
func TestUnsupportedUnloadDoesNotRetry(t *testing.T) {
	r := &unsupportedReleaseRuntime{}
	c := testController(t, openStore(t), r)
	err := c.waitReleased(context.Background(), time.Now().Add(20*time.Millisecond))
	if !errors.Is(err, gpuruntime.ErrUnloadUnverified) || errors.Is(err, ErrVerifyTimeout) || r.probes != 1 {
		t.Fatalf("unsupported verification retried %d: %v", r.probes, err)
	}
}

func (r *unsupportedReleaseRuntime) Healthy(context.Context, control.Workload) error {
	r.probes++
	return gpuruntime.ErrUnloadUnverified
}
func TestUnsupportedUnloadReadinessDoesNotRetry(t *testing.T) {
	r := &unsupportedReleaseRuntime{fakeRuntime: fakeRuntime{active: control.WorkloadText}}
	c := testController(t, openStore(t), r)
	err := c.waitReady(context.Background(), control.WorkloadText, time.Now().Add(20*time.Millisecond))
	if !errors.Is(err, gpuruntime.ErrUnloadUnverified) || errors.Is(err, ErrVerifyTimeout) || r.probes != 1 {
		t.Fatalf("unsupported readiness retried %d: %v", r.probes, err)
	}
}
