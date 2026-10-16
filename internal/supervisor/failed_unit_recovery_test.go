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

type failedUnitRecoveryRuntime struct {
	fakeRuntime
	failed               control.Workload
	recoveryObservations int
}

func (r *failedUnitRecoveryRuntime) Observe(ctx context.Context) (gpuruntime.Snapshot, error) {
	if r.failed != "" {
		return gpuruntime.Snapshot{}, errors.New("unit is failed/failed")
	}
	return r.fakeRuntime.Observe(ctx)
}

func (r *failedUnitRecoveryRuntime) ObserveForRecovery(ctx context.Context) (gpuruntime.Snapshot, error) {
	r.recoveryObservations++
	snap, err := r.fakeRuntime.Observe(ctx)
	if err == nil && r.failed != "" {
		o := snap.Workloads[r.failed]
		o.Active = false
		snap.Workloads[r.failed] = o
	}
	return snap, err
}

func (r *failedUnitRecoveryRuntime) Stop(ctx context.Context, id control.Workload) error {
	if err := r.fakeRuntime.Stop(ctx, id); err != nil {
		return err
	}
	if r.failed == id {
		r.failed = ""
	}
	return nil
}

func TestRecoverFailedRetainUnitDrainsBeforeCleanup(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	r := &failedUnitRecoveryRuntime{fakeRuntime: fakeRuntime{active: control.WorkloadText}}
	c := testController(t, s, r)
	before, err := c.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.AdmitWorkToken(ctx, "crashed-work", "", control.WorkloadText, before.LeaseFence)
	if err != nil {
		t.Fatal(err)
	}
	r.failed = control.WorkloadText
	r.calls = nil
	r.releaseCalls = 0
	if _, err := c.Reconcile(ctx); err == nil || r.recoveryObservations != 0 {
		t.Fatal("ordinary reconciliation bypassed strict observation")
	}
	c.config.DrainTimeout = 5 * time.Millisecond
	if _, err := c.Recover(ctx); !errors.Is(err, ErrDrainTimeout) {
		t.Fatalf("pending failed work recovery: %v", err)
	}
	if len(r.calls) != 0 || r.failed == "" {
		t.Fatalf("cleanup before drain: %v", r.calls)
	}
	if err := s.FinishWorkToken(ctx, "crashed-work", control.WorkloadText, before.LeaseFence, token, store.WorkCompleted); err != nil {
		t.Fatal(err)
	}
	after, err := c.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.ActiveWorkload != control.WorkloadIdle || after.Health != control.HealthHealthy || r.releaseCalls == 0 {
		t.Fatalf("failed retained unit recovery: %#v, release calls=%d", after, r.releaseCalls)
	}
	assertCalls(t, r.calls, "stop text", "stop media")
}

func TestRecoverFailedUnitCannotBypassCleanupOrRelease(t *testing.T) {
	for _, failure := range []string{"stop/reset", "release"} {
		t.Run(failure, func(t *testing.T) {
			s := openStore(t)
			r := &failedUnitRecoveryRuntime{failed: control.WorkloadText}
			c := testController(t, s, r)
			c.config.ActionTimeout = 5 * time.Millisecond
			c.config.VerifyTimeout = 10 * time.Millisecond
			if failure == "stop/reset" {
				r.stopErr = errors.New("cleanup failed")
			} else {
				r.blockRelease = true
			}
			if _, err := c.Recover(context.Background()); err == nil {
				t.Fatal("failed cleanup/release reopened admission")
			}
			after, err := s.State(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if after.Admission != control.AdmissionClosed || after.Health != control.HealthError || after.ActiveWorkload != control.WorkloadUnknown {
				t.Fatalf("unsafe recovery state: %#v", after)
			}
		})
	}
}

func TestRecoverFailedOpposingUnitPreservesHealthyRetainedWork(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	r := &failedUnitRecoveryRuntime{fakeRuntime: fakeRuntime{active: control.WorkloadText}}
	c := testController(t, s, r)
	before, err := c.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.AdmitWorkToken(ctx, "retained-work", "", control.WorkloadText, before.LeaseFence)
	if err != nil {
		t.Fatal(err)
	}
	r.failed = control.WorkloadMedia
	r.calls = nil
	after, err := c.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.ActiveWorkload != control.WorkloadText || after.Admission != control.AdmissionOpen {
		t.Fatalf("healthy retained workload lost: %#v", after)
	}
	assertCalls(t, r.calls, "stop media")
	if err := s.FinishWorkToken(ctx, "retained-work", control.WorkloadText, before.LeaseFence, token, store.WorkCompleted); err != nil {
		t.Fatal(err)
	}
}
