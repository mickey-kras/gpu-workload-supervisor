package supervisor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/proxy"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

type observedHandoffStore struct {
	*store.Store
	attempt chan struct{}
}

func (s observedHandoffStore) AcquireUserExecution(ctx context.Context, shared bool) (*lock.File, error) {
	if !shared {
		close(s.attempt)
	}
	return s.Store.AcquireUserExecution(ctx, shared)
}

func TestHTTPUserHandoffWaitsForForwardingWithoutRestartingStoppedWork(t *testing.T) {
	for _, failure := range []string{"none", "handoff timeout", "target start"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			stateStore := openStore(t)
			ownershipState(t, stateStore, control.OwnerUser, control.WorkloadText)
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get(proxy.DefaultRegistrationTokenHeader) != "" {
					t.Error("User request was registered")
				}
				close(entered)
				select {
				case <-release:
				case <-r.Context().Done():
				}
				w.WriteHeader(http.StatusAccepted)
			}))
			defer upstream.Close()
			target, err := url.Parse(upstream.URL)
			if err != nil {
				t.Fatal(err)
			}
			handler, err := proxy.New(stateStore, proxy.Config{Upstream: target, Workload: control.WorkloadText, ExecutionRoutes: []proxy.Route{{Method: "POST", Path: "/execute"}}})
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(handler)
			defer server.Close()
			defer unblock()
			requestDone := make(chan error, 1)
			go func() {
				response, err := server.Client().Post(server.URL+"/execute", "application/json", nil)
				if err == nil {
					response.Body.Close()
				}
				requestDone <- err
			}()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("User request did not dispatch")
			}
			runtime := &fakeRuntime{active: control.WorkloadText, mediaReady: true}
			if failure == "target start" {
				runtime.startErr = errors.New("target unavailable")
			}
			observed := observedHandoffStore{Store: stateStore, attempt: make(chan struct{})}
			controller := testController(t, observed, runtime)
			controller.config.DrainTimeout = time.Second
			if failure == "handoff timeout" {
				controller.config.DrainTimeout = 20 * time.Millisecond
			}
			type result struct {
				state control.State
				err   error
			}
			done := make(chan result, 1)
			go func() {
				state, err := controller.TransferToSupervisor(ctx, control.WorkloadMedia, "proxy-contract")
				done <- result{state, err}
			}()
			select {
			case <-observed.attempt:
			case <-time.After(3 * time.Second):
				t.Fatal("handoff gate not reached")
			}
			if failure != "handoff timeout" {
				select {
				case got := <-done:
					t.Fatalf("handoff escaped active forwarding: %#v", got)
				default:
				}
				unblock()
			}
			var got result
			select {
			case got = <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("handoff did not finish")
			}
			unblock()
			if err := <-requestDone; err != nil {
				t.Fatal(err)
			}
			if failure == "none" {
				if got.err != nil || got.state.Owner != control.OwnerSupervisor || got.state.ActiveWorkload != control.WorkloadMedia {
					t.Fatalf("handoff result %#v %v", got.state, got.err)
				}
			} else if got.err == nil || got.state.Owner != control.OwnerUser || got.state.Health != control.HealthError || got.state.Admission != control.AdmissionClosed {
				t.Fatalf("failed handoff %#v %v", got.state, got.err)
			}
			for _, call := range runtime.calls {
				if call == "start text" {
					t.Fatal("rollback restarted stopped User work")
				}
			}
			if failure == "handoff timeout" && !errors.Is(got.err, context.DeadlineExceeded) {
				t.Fatalf("timeout error %v", got.err)
			}
		})
	}
}
