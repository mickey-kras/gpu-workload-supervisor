package proxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

type rejectingDefaultTransport struct{ calls int }

func (t *rejectingDefaultTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.calls++
	return &http.Response{StatusCode: http.StatusBadGateway, Header: make(http.Header), Body: http.NoBody, Request: r}, nil
}

func TestForwardingDoesNotUseGlobalDefaultTransport(t *testing.T) {
	poisoned := &rejectingDefaultTransport{}
	original := http.DefaultTransport
	http.DefaultTransport = poisoned
	t.Cleanup(func() { http.DefaultTransport = original })
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusAccepted) }))
	defer upstream.Close()
	for _, scenario := range []struct {
		name, method, path string
		owner              control.Owner
	}{
		{"supervisor execution", "POST", "/execute", control.OwnerSupervisor},
		{"user execution", "POST", "/execute", control.OwnerUser},
		{"read only", "GET", "/assets/item", control.OwnerSupervisor},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			state := admittedState(scenario.owner)
			handler := testHandler(t, &fakeStore{state: state}, upstream.URL)
			request := httptest.NewRequest(scenario.method, scenario.path, nil)
			addLeaseHeaders(request, state.LeaseFence)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusAccepted {
				t.Fatalf("forwarding status = %d, want 202", response.Code)
			}
		})
	}
	if poisoned.calls != 0 {
		t.Fatalf("forwarding used global default transport %d times", poisoned.calls)
	}
}

func TestExplicitTransportPreservesUpstreamTLSConfiguration(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusAccepted) }))
	defer upstream.Close()
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	state := admittedState(control.OwnerSupervisor)
	handler, err := New(&fakeStore{state: state}, Config{
		Upstream: target, Workload: control.WorkloadMedia,
		ExecutionRoutes: []Route{{Method: http.MethodPost, Path: "/execute"}},
		Transport:       upstream.Client().Transport,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/execute", nil)
	addLeaseHeaders(request, state.LeaseFence)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("TLS forwarding status = %d, want 202", response.Code)
	}
}
