package supervisor

import (
	"context"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
	"strings"
	"testing"
)

type cancelAtFinishStore struct {
	*store.Store
	cancel context.CancelFunc
}

func (s cancelAtFinishStore) FinishTransition(ctx context.Context, id, status string, version uint64, state control.State) (control.State, error) {
	if status == "committed" {
		s.cancel()
	}
	return s.Store.FinishTransition(ctx, id, status, version, state)
}
func TestVerifiedTransitionFinalizesAfterCallerCancellation(t *testing.T) {
	stateStore := openStore(t)
	ownershipState(t, stateStore, control.OwnerSupervisor, control.WorkloadText)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	controller := testController(t, cancelAtFinishStore{stateStore, cancel}, &fakeRuntime{active: control.WorkloadText, mediaReady: true})
	state, err := controller.Switch(ctx, control.WorkloadMedia, "test")
	if err != nil {
		t.Fatal(err)
	}
	if state.Phase != control.PhaseStable || state.Admission != control.AdmissionOpen || state.ActiveWorkload != control.WorkloadMedia {
		t.Fatalf("inconsistent state: %#v", state)
	}
	running, err := stateStore.InProgressTransition(context.Background())
	if err != nil || running != "" {
		t.Fatalf("unfinished transition %q: %v", running, err)
	}
}

type deadlineRuntime struct{ fakeRuntime }

func (r *deadlineRuntime) Observe(context.Context) (gpuruntime.Snapshot, error) {
	return gpuruntime.Snapshot{}, gpuruntime.SafeError("probe failed", context.DeadlineExceeded)
}
func TestObservationWrappingPreservesTimeout(t *testing.T) {
	c := testController(t, openStore(t), &deadlineRuntime{})
	if err := c.checkReady(context.Background(), control.WorkloadText); !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrRuntimeObservation) || failureCode(err) != "timeout" || strings.Contains(err.Error(), "secret") {
		t.Fatalf("wrapped timeout: %v", err)
	}
	if _, err := c.unloadCatalog(context.Background(), "", control.PhaseUnloading, control.State{}, control.WorkloadMedia); !errors.Is(err, context.DeadlineExceeded) || failureCode(err) != "timeout" || strings.Contains(err.Error(), "secret") {
		t.Fatalf("wrapped timeout: %v", err)
	}
}

type rejectFinishStore struct{ *store.Store }

func (s rejectFinishStore) FinishTransition(context.Context, string, string, uint64, control.State) (control.State, error) {
	return control.State{}, store.ErrVersionConflict
}
func TestVerifiedFinalizationDoesNotHideVersionConflict(t *testing.T) {
	s := openStore(t)
	ownershipState(t, s, control.OwnerSupervisor, control.WorkloadText)
	r := &fakeRuntime{active: control.WorkloadText, mediaReady: true}
	c := testController(t, rejectFinishStore{s}, r)
	if _, err := c.Switch(context.Background(), control.WorkloadMedia, "test"); !errors.Is(err, store.ErrVersionConflict) {
		t.Fatalf("finish conflict: %v", err)
	}
	after, _ := s.State(context.Background())
	if after.Admission != control.AdmissionClosed || after.Phase != control.PhaseVerifying {
		t.Fatalf("ambiguous finish reopened state: %#v", after)
	}
}
