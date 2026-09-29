package proxy

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

type unavailableStore struct{ *fakeStore }

func (*unavailableStore) State(context.Context) (control.State, error) {
	return control.State{}, errors.New("state unavailable")
}

func TestConfigRejectsInvalidOriginsAndRoutes(t *testing.T) {
	target, err := url.Parse("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	valid := Config{Upstream: target, Workload: control.WorkloadMedia,
		ExecutionRoutes: []Route{{Method: "POST", Path: "/execute"}}}
	if _, err := New(nil, valid); err == nil {
		t.Fatal("nil state store accepted")
	}
	tests := []struct {
		name string
		edit func(*Config)
	}{
		{"missing upstream", func(config *Config) { config.Upstream = nil }},
		{"unsupported scheme", func(config *Config) { config.Upstream = &url.URL{Scheme: "file", Host: "localhost"} }},
		{"invalid workload", func(config *Config) { config.Workload = control.WorkloadIdle }},
		{"no execution route", func(config *Config) { config.ExecutionRoutes = nil }},
		{"invalid execution route", func(config *Config) { config.ExecutionRoutes = []Route{{Method: "POST", Path: "/x/../execute"}} }},
		{"invalid passthrough route", func(config *Config) { config.PassthroughRoutes = []Route{{Method: "", Path: "/passthrough"}} }},
		{"invalid completion path", func(config *Config) { config.CompletionPath = "finish" }},
		{"request ID overlaps token", func(config *Config) { config.RequestIDHeader = DefaultRegistrationTokenHeader }},
		{"fence headers overlap", func(config *Config) { config.FenceEpochHeader = DefaultFenceIDHeader }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.edit(&config)
			if err := ValidateConfig(config); err == nil {
				t.Fatal("invalid proxy configuration accepted")
			}
		})
	}
	if err := ValidateConfig(valid); err != nil {
		t.Fatal(err)
	}
}

func TestExecutionRejectsStateAndFenceFailuresBeforeUpstream(t *testing.T) {
	tests := []struct {
		name  string
		state StateStore
		edit  func(*http.Request)
		want  int
	}{
		{"unavailable state", &unavailableStore{&fakeStore{}}, nil, http.StatusServiceUnavailable},
		{"unknown owner", &fakeStore{state: admittedState("unknown")}, nil, http.StatusServiceUnavailable},
		{"bad epoch", &fakeStore{state: admittedState(control.OwnerSupervisor)}, func(request *http.Request) {
			request.Header.Set(DefaultFenceEpochHeader, "not-an-epoch")
		}, http.StatusBadRequest},
		{"missing incarnation", &fakeStore{state: admittedState(control.OwnerSupervisor)}, func(request *http.Request) {
			request.Header.Del(DefaultFenceIDHeader)
		}, http.StatusBadRequest},
		{"store unavailable", &fakeStore{state: admittedState(control.OwnerSupervisor), admitErr: errors.New("database unavailable")}, nil, http.StatusServiceUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := testHandler(t, test.state, "http://127.0.0.1:1")
			request := httptest.NewRequest(http.MethodPost, "/execute", nil)
			addLeaseHeaders(request, admittedState(control.OwnerSupervisor).LeaseFence)
			if test.edit != nil {
				test.edit(request)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d", response.Code, test.want)
			}
		})
	}
}

func TestFinishRejectsMalformedAndUnknownWork(t *testing.T) {
	tests := []struct {
		name, body string
		finishErr  error
		want       int
	}{
		{"invalid JSON", "{", nil, http.StatusBadRequest},
		{"missing request", `{"fence":{"incarnation":"11111111-1111-4111-8111-111111111111","epoch":7},"outcome":"completed"}`, nil, http.StatusBadRequest},
		{"invalid outcome", `{"requestId":"request","fence":{"incarnation":"11111111-1111-4111-8111-111111111111","epoch":7},"outcome":"other"}`, nil, http.StatusBadRequest},
		{"missing token", `{"requestId":"request","fence":{"incarnation":"11111111-1111-4111-8111-111111111111","epoch":7},"outcome":"completed"}`, nil, http.StatusConflict},
		{"unknown request", `{"requestId":"request","fence":{"incarnation":"11111111-1111-4111-8111-111111111111","epoch":7},"outcome":"completed"}`, sql.ErrNoRows, http.StatusNotFound},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stateStore := &fakeStore{state: admittedState(control.OwnerSupervisor), finishErr: test.finishErr}
			handler := testHandler(t, stateStore, "http://127.0.0.1:1")
			request := httptest.NewRequest(http.MethodPost, DefaultCompletionPath, strings.NewReader(test.body))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want || len(stateStore.finished) != 0 {
				t.Fatalf("status = %d, finished = %#v", response.Code, stateStore.finished)
			}
		})
	}
}

func TestPassthroughMutationsRemoveControlHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get(DefaultFenceIDHeader) != "" {
			t.Error("lease header reached upstream")
		}
		if request.Header.Get(DefaultRegistrationTokenHeader) != "" {
			t.Error("registration token reached passthrough upstream")
		}
		response.WriteHeader(http.StatusAccepted)
	}))
	defer upstream.Close()
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(&fakeStore{state: admittedState(control.OwnerSupervisor)}, Config{
		Upstream: target, Workload: control.WorkloadMedia,
		ExecutionRoutes:   []Route{{Method: http.MethodPost, Path: "/execute"}},
		PassthroughRoutes: []Route{{Method: http.MethodPost, Path: "/control"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/control", nil)
	addLeaseHeaders(request, admittedState(control.OwnerSupervisor).LeaseFence)
	request.Header.Set(DefaultRegistrationTokenHeader, "caller-supplied")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d", response.Code)
	}
}
