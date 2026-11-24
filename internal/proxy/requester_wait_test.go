package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mickey-kras/llm-workload-supervisor/internal/control"
)

// waitHandler serves requester demand against s with a fast poll and an
// upstream that accepts every dispatched request.
func waitHandler(t *testing.T, s StateStore, c Config) (*Handler, *atomic.Int32) {
	t.Helper()
	var dispatched atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		dispatched.Add(1)
		w.WriteHeader(202)
	}))
	t.Cleanup(upstream.Close)
	c.Upstream, _ = url.Parse(upstream.URL)
	h, err := NewWithContext(context.Background(), s, c)
	if err != nil {
		t.Fatal(err)
	}
	h.demandPoll = 5 * time.Millisecond
	return h, &dispatched
}

func setRequesterState(s *requesterStore, change func(*control.State)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	change(&s.state)
}

func idleRequesterState(state *control.State) {
	state.ActiveWorkload, state.DesiredWorkload = control.WorkloadIdle, control.WorkloadIdle
	state.Admission, state.Phase = control.AdmissionClosed, control.PhaseStable
}

func TestRequesterDemandWaitTimeoutCancelsDemand(t *testing.T) {
	f := newContractFixture(t)
	ctx := context.Background()
	state := f.state
	idleRequesterState(&state)
	var err error
	if f.state, err = f.store.UpdateState(ctx, state.Version, state); err != nil {
		t.Fatal(err)
	}
	c := requesterConfig()
	var calls atomic.Int32
	c.ActivateDemand = func(context.Context, control.Workload, control.State, string) (control.Fence, error) {
		calls.Add(1)
		return control.Fence{}, ErrActivationRetry
	}
	h, dispatched := waitHandler(t, f.store, c)
	r := requesterRequest()
	r.Header.Set(DefaultRequestIDHeader, "reservation-request")
	w := httptest.NewRecorder()
	started := time.Now()
	h.ServeHTTP(w, r)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "demand_timeout") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if time.Since(started) < c.DemandWaitTimeout || calls.Load() < 2 || dispatched.Load() != 0 {
		t.Fatalf("wait ended early: calls=%d dispatched=%d", calls.Load(), dispatched.Load())
	}
	assertDemandReservation(t, f, "cancelled", 0)
}

func TestRequesterDemandFailsFastOnRefusedActivation(t *testing.T) {
	s := &requesterStore{fakeStore: fakeStore{state: admittedState(control.OwnerSupervisor)}}
	idleRequesterState(&s.state)
	c := requesterConfig()
	c.DemandWaitTimeout = time.Minute
	var calls atomic.Int32
	c.ActivateDemand = func(context.Context, control.Workload, control.State, string) (control.Fence, error) {
		calls.Add(1)
		return control.Fence{}, context.DeadlineExceeded
	}
	h, dispatched := waitHandler(t, s, c)
	w := httptest.NewRecorder()
	started := time.Now()
	h.ServeHTTP(w, requesterRequest())
	if w.Code != 409 || !strings.Contains(w.Body.String(), "demand_rejected") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if time.Since(started) > 5*time.Second || calls.Load() != 1 || s.reserved != 0 || dispatched.Load() != 0 {
		t.Fatalf("refusal did not fail fast: calls=%d reserved=%d", calls.Load(), s.reserved)
	}
}

func TestRequesterDemandSwitchWaitsForOtherWorkloadWork(t *testing.T) {
	s := &requesterStore{fakeStore: fakeStore{state: admittedState(control.OwnerSupervisor)}}
	s.state.ActiveWorkload, s.state.DesiredWorkload = control.WorkloadText, control.WorkloadText
	c := requesterConfig()
	c.DemandWaitTimeout = time.Minute
	var calls atomic.Int32
	c.ActivateDemand = func(_ context.Context, target control.Workload, state control.State, _ string) (control.Fence, error) {
		// The supervisor defers the switch while text work is in flight.
		if calls.Add(1) < 3 {
			return control.Fence{}, ErrActivationRetry
		}
		if state.ActiveWorkload != control.WorkloadText || target != control.WorkloadMedia {
			t.Errorf("switch from %s to %s", state.ActiveWorkload, target)
		}
		setRequesterState(s, func(state *control.State) { *state = admittedState(control.OwnerSupervisor) })
		return control.Fence{}, nil
	}
	h, dispatched := waitHandler(t, s, c)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, requesterRequest())
	if w.Code != 202 || calls.Load() != 3 || s.reserved != 1 || dispatched.Load() != 1 {
		t.Fatalf("%d calls=%d reserved=%d dispatched=%d %s", w.Code, calls.Load(), s.reserved, dispatched.Load(), w.Body.String())
	}
}

func TestRequesterConcurrentDemandsActivateOnce(t *testing.T) {
	s := &requesterStore{fakeStore: fakeStore{state: admittedState(control.OwnerSupervisor)}}
	idleRequesterState(&s.state)
	c := requesterConfig()
	c.DemandWaitTimeout = time.Minute
	var starts atomic.Int32
	c.ActivateDemand = func(_ context.Context, _ control.Workload, seen control.State, _ string) (control.Fence, error) {
		// Like the store, a stale version or a running transition is retried.
		s.mu.Lock()
		if s.state.Version != seen.Version || s.state.Phase != control.PhaseStable {
			s.mu.Unlock()
			return control.Fence{}, ErrActivationRetry
		}
		starts.Add(1)
		s.state.Phase, s.state.Version = control.PhaseDraining, s.state.Version+1
		s.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		setRequesterState(s, func(state *control.State) {
			version := state.Version
			*state = admittedState(control.OwnerSupervisor)
			state.Version = version + 1
		})
		return control.Fence{}, nil
	}
	h, dispatched := waitHandler(t, s, c)
	var wg sync.WaitGroup
	codes := make([]int, 4)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			h.ServeHTTP(w, requesterRequest())
			codes[i] = w.Code
		}()
	}
	wg.Wait()
	for _, code := range codes {
		if code != 202 {
			t.Fatalf("codes = %v", codes)
		}
	}
	if starts.Load() != 1 || dispatched.Load() != 4 {
		t.Fatalf("starts=%d dispatched=%d", starts.Load(), dispatched.Load())
	}
}

func TestRequesterDemandWakesAfterIdleTransition(t *testing.T) {
	s := &requesterStore{fakeStore: fakeStore{state: admittedState(control.OwnerSupervisor)}}
	// The inactivity policy is stopping media to Idle.
	s.state.DesiredWorkload, s.state.Admission, s.state.Phase = control.WorkloadIdle, control.AdmissionClosed, control.PhaseDraining
	c := requesterConfig()
	c.DemandWaitTimeout = time.Minute
	var calls atomic.Int32
	c.ActivateDemand = func(_ context.Context, _ control.Workload, state control.State, _ string) (control.Fence, error) {
		calls.Add(1)
		if state.ActiveWorkload != control.WorkloadIdle || state.Phase != control.PhaseStable {
			t.Errorf("activation during the idle stop: %+v", state)
		}
		setRequesterState(s, func(state *control.State) { *state = admittedState(control.OwnerSupervisor) })
		return control.Fence{}, nil
	}
	h, dispatched := waitHandler(t, s, c)
	go func() {
		time.Sleep(50 * time.Millisecond)
		setRequesterState(s, idleRequesterState)
	}()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, requesterRequest())
	if w.Code != 202 || calls.Load() != 1 || dispatched.Load() != 1 {
		t.Fatalf("%d calls=%d dispatched=%d %s", w.Code, calls.Load(), dispatched.Load(), w.Body.String())
	}
}

func TestRequesterSlowActivationKeepsTheWaitBudget(t *testing.T) {
	s := &requesterStore{fakeStore: fakeStore{state: admittedState(control.OwnerSupervisor)}}
	idleRequesterState(&s.state)
	c := requesterConfig()
	c.DemandWaitTimeout = 20 * time.Millisecond
	c.ActivateDemand = func(context.Context, control.Workload, control.State, string) (control.Fence, error) {
		// The operator runs activation on its own context, past the wait.
		time.Sleep(60 * time.Millisecond)
		setRequesterState(s, func(state *control.State) { *state = admittedState(control.OwnerSupervisor) })
		return control.Fence{}, nil
	}
	h, dispatched := waitHandler(t, s, c)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, requesterRequest())
	if w.Code != 409 || !strings.Contains(w.Body.String(), "demand_timeout") || s.reserved != 0 || dispatched.Load() != 0 {
		t.Fatalf("late activation dispatched: %d reserved=%d dispatched=%d %s", w.Code, s.reserved, dispatched.Load(), w.Body.String())
	}
}
