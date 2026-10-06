package supervisor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/proxy"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

type recoverySafetyRuntime struct {
	fakeRuntime
	observeErr error
	onObserve  func()
}

func (r *recoverySafetyRuntime) Observe(ctx context.Context) (gpuruntime.Snapshot, error) {
	if r.onObserve != nil {
		r.onObserve()
	}
	if r.observeErr != nil {
		return gpuruntime.Snapshot{}, r.observeErr
	}
	return r.fakeRuntime.Observe(ctx)
}

func TestRecoverInitiallyOpenFailureClosesAdmission(t *testing.T) {
	for _, failure := range []string{"observation", "stop", "release", "health", "cancellation"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s := openStore(t)
			r := &recoverySafetyRuntime{fakeRuntime: fakeRuntime{active: control.WorkloadText}}
			c := testController(t, s, r)
			before, err := c.Reconcile(ctx)
			if err != nil {
				t.Fatal(err)
			}
			c.config.ActionTimeout = 5 * time.Millisecond
			c.config.VerifyTimeout = 10 * time.Millisecond
			switch failure {
			case "observation":
				r.observeErr = errors.New("observation failed")
			case "health":
				r.healthFailures = 1
			case "stop":
				r.active = control.WorkloadMedia
				r.stopErr = errors.New("stop failed")
			case "release":
				r.active = control.WorkloadMedia
				r.blockRelease = true
			case "cancellation":
				r.onObserve = cancel
				r.observeErr = context.Canceled
			}
			if failure == "stop" || failure == "release" {
				before.ActiveWorkload = control.WorkloadMedia
				before.DesiredWorkload = control.WorkloadMedia
				before, err = s.UpdateState(ctx, before.Version, before)
				if err != nil {
					t.Fatal(err)
				}
			}
			_, err = c.Recover(ctx)
			if err == nil {
				t.Fatal("recovery unexpectedly succeeded")
			}
			after, err := s.State(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if after.Admission != control.AdmissionClosed || after.Health != control.HealthError || after.ActiveWorkload != control.WorkloadUnknown || after.Phase != control.PhaseReconciling {
				t.Fatalf("failed recovery left unsafe state: %#v", after)
			}
			if after.LeaseFence == before.LeaseFence {
				t.Fatal("recovery did not fence old admissions")
			}
			if _, err := s.AdmitWorkToken(context.Background(), "after-failure", "", control.WorkloadText, after.LeaseFence); !errors.Is(err, store.ErrAdmissionClosed) {
				t.Fatalf("admission after failure = %v", err)
			}
			upstream, _ := url.Parse("http://127.0.0.1:1")
			handler, err := proxy.NewWithContext(context.Background(), s, proxy.Config{Upstream: upstream, Workload: before.ActiveWorkload, ExecutionRoutes: []proxy.Route{{Method: http.MethodPost, Path: "/execute"}}})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/execute", nil)
			request.Header.Set(proxy.DefaultRequestIDHeader, "proxy-after-failure")
			request.Header.Set(proxy.DefaultFenceIDHeader, after.LeaseFence.Incarnation)
			request.Header.Set(proxy.DefaultFenceEpochHeader, strconv.FormatUint(after.LeaseFence.Epoch, 10))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("proxy admission after failure = %d: %s", response.Code, response.Body.String())
			}

		})
	}
}

func TestRecoverDrainsBeforeDestructiveStop(t *testing.T) {
	s := openStore(t)
	r := &recoverySafetyRuntime{fakeRuntime: fakeRuntime{active: control.WorkloadText}}
	c := testController(t, s, r)
	before, err := c.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.AdmitWorkToken(context.Background(), "already-admitted", "", control.WorkloadText, before.LeaseFence)
	if err != nil {
		t.Fatal(err)
	}
	r.active = control.WorkloadMedia
	c.config.DrainTimeout = 5 * time.Millisecond
	if _, err := c.Recover(context.Background()); !errors.Is(err, ErrDrainTimeout) {
		t.Fatalf("recovery with admitted work = %v", err)
	}
	if len(r.calls) != 0 {
		t.Fatalf("recovery stopped runtime before draining: %v", r.calls)
	}
	if err := s.FinishWorkToken(context.Background(), "already-admitted", control.WorkloadText, before.LeaseFence, token, store.WorkCompleted); err != nil {
		t.Fatal(err)
	}
	after, err := c.Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if after.ActiveWorkload != control.WorkloadIdle || after.Health != control.HealthHealthy {
		t.Fatalf("drained recovery = %#v", after)
	}
}

type cancelAfterRecoveryRead struct {
	storeGateway
	cancel context.CancelFunc
}

func (s *cancelAfterRecoveryRead) State(ctx context.Context) (control.State, error) {
	state, err := s.storeGateway.State(ctx)
	s.cancel()
	return state, err
}

func TestRecoverCancellationBeforeEntryStillClosesAdmission(t *testing.T) {
	s := openStore(t)
	r := &fakeRuntime{active: control.WorkloadText}
	c := testController(t, s, r)
	if _, err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.store = &cancelAfterRecoveryRead{storeGateway: s, cancel: cancel}
	if _, err := c.Recover(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("recovery = %v", err)
	}
	after, err := s.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if after.Admission != control.AdmissionClosed || after.Health != control.HealthError || after.ActiveWorkload != control.WorkloadUnknown {
		t.Fatalf("canceled entry left unsafe state: %#v", after)
	}
}
