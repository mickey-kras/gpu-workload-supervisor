package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

// Deliberately nonrandom fixtures: valid wire format, not production secrets.
const executionCredential = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const completionCredential = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func adapterConfig(target *url.URL) Config {
	return Config{Upstream: target, Workload: control.WorkloadMedia,
		ExecutionRoutes:   []Route{{Method: "POST", Path: "/execute"}},
		ReadOnlyRoutes:    []Route{{Method: "GET", Path: "/monitor"}},
		PassthroughRoutes: []Route{{Method: "PATCH", Path: "/edit"}},
		IngressMode:       IngressAdapter, ExecutionCredential: executionCredential, CompletionCredential: completionCredential}
}

func adapterHandler(t *testing.T, stateStore StateStore, upstream string) *Handler {
	t.Helper()
	target, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewWithContext(context.Background(), stateStore, adapterConfig(target))
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func TestAdapterConfigFailsClosed(t *testing.T) {
	target, _ := url.Parse("http://127.0.0.1:1")
	for _, test := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"missing execution", func(c *Config) { c.ExecutionCredential = "" }},
		{"missing completion", func(c *Config) { c.CompletionCredential = "" }},
		{"malformed", func(c *Config) { c.ExecutionCredential = strings.Repeat("g", 64) }},
		{"short", func(c *Config) { c.ExecutionCredential = "abc" }},
		{"equal", func(c *Config) { c.CompletionCredential = c.ExecutionCredential }},
		{"unknown mode", func(c *Config) { c.IngressMode = "unknown" }},
		{"trusted with credentials", func(c *Config) { c.IngressMode = IngressTrusted }},
		{"default with credentials", func(c *Config) { c.IngressMode = "" }},
		{"url credentials", func(c *Config) { u := *target; u.User = url.UserPassword("user", "password"); c.Upstream = &u }},
		{"id alias", func(c *Config) { c.RequestIDHeader = strings.ToLower(ExecutionCredentialHeader) }},
		{"job alias", func(c *Config) { c.JobIDHeader = CompletionCredentialHeader }},
		{"auth alias", func(c *Config) { c.FenceIDHeader = "Authorization" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := adapterConfig(target)
			test.mutate(&config)
			if _, err := NewWithContext(context.Background(), &fakeStore{}, config); err == nil {
				t.Fatal("unsafe ingress accepted")
			}
		})
	}
}

type unreadableBody struct{ t *testing.T }

func (b unreadableBody) Read([]byte) (int, error) {
	b.t.Error("unauthorized body was read")
	return 0, io.EOF
}
func (b unreadableBody) Close() error { return nil }

// Every store operation panics: unauthorized requests must not reach state,
// registration, the user handoff gate or completion.
type untouchedStore struct{ StateStore }

func TestAdapterAuthorizesBeforeStateAndBody(t *testing.T) {
	handler := adapterHandler(t, untouchedStore{}, "http://127.0.0.1:1")
	for _, route := range []Route{{"POST", "/execute"}, {"GET", "/monitor"}, {"PATCH", "/edit"}, {"POST", DefaultCompletionPath}, {"GET", DefaultCompletionPath}, {"GET", "/unknown"}, {"POST", "/a/../execute"}} {
		for _, credentials := range [][]string{nil, {"forged"}, {executionCredential}, {completionCredential}, {executionCredential, executionCredential}} {
			request := httptest.NewRequest(route.Method, route.Path, unreadableBody{t})
			// Deliberately send the wrong role to each endpoint.
			header := CompletionCredentialHeader
			if route.Method == "POST" && route.Path == DefaultCompletionPath {
				header = ExecutionCredentialHeader
			}
			for _, credential := range credentials {
				request.Header.Add(header, credential)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s: %d", route.Method, route.Path, response.Code)
			}
		}
	}
	for _, values := range [][]string{{executionCredential, executionCredential}, {executionCredential + ", " + executionCredential}, {" " + executionCredential}, {executionCredential + " "}, {"forged"}} {
		request := httptest.NewRequest("POST", "/execute", unreadableBody{t})
		request.Header[ExecutionCredentialHeader] = values
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("ambiguous credential: %d", response.Code)
		}
	}
	request := httptest.NewRequest("POST", "/execute", unreadableBody{t})
	request.Header.Set(ExecutionCredentialHeader, executionCredential)
	request.Header.Set(CompletionCredentialHeader, completionCredential)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatal("combined roles accepted")
	}
}

func TestAdapterSuppressesUpstreamAndClientMetadata(t *testing.T) {
	for _, route := range []Route{{"POST", "/execute"}, {"GET", "/monitor"}, {"PATCH", "/edit"}} {
		t.Run(route.Method, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for _, name := range []string{ExecutionCredentialHeader, CompletionCredentialHeader, "Authorization", "Proxy-Authorization"} {
					if r.Header.Get(name) != "" {
						t.Errorf("upstream received %s", name)
					}
					w.Header().Set(name, "must-not-leak")
					w.Header().Add("Trailer", name)
				}
				if route.Path == "/execute" && r.Header.Get(DefaultRegistrationTokenHeader) != "test-registration-token" {
					t.Error("upstream lost registered tuple")
				}
				w.Header().Set(DefaultRegistrationTokenHeader, "must-not-leak")
				w.Header().Add("Trailer", DefaultRegistrationTokenHeader)
				w.Header().Add("Trailer", "X-Result")
				w.WriteHeader(http.StatusAccepted)
				_, _ = io.WriteString(w, "accepted")
				for _, name := range []string{ExecutionCredentialHeader, CompletionCredentialHeader, "Authorization", "Proxy-Authorization", DefaultRegistrationTokenHeader} {
					w.Header().Set(name, "late-must-not-leak")
				}
				w.Header().Set("X-Result", "safe-trailer")
			}))
			defer upstream.Close()
			stateStore := &fakeStore{state: admittedState(control.OwnerSupervisor)}
			proxy := httptest.NewServer(adapterHandler(t, stateStore, upstream.URL))
			defer proxy.Close()
			request, _ := http.NewRequest(route.Method, proxy.URL+route.Path, nil)
			addLeaseHeaders(request, stateStore.state.LeaseFence)
			request.Header.Set(ExecutionCredentialHeader, executionCredential)
			request.Header.Set("Authorization", "client-secret")
			request.Header.Set("Proxy-Authorization", "client-proxy-secret")
			request.Header.Set("Connection", ExecutionCredentialHeader+", "+DefaultRegistrationTokenHeader+", "+DefaultRequestIDHeader)
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusAccepted || string(body) != "accepted" {
				t.Fatalf("response %d %q", response.StatusCode, body)
			}
			for _, name := range []string{ExecutionCredentialHeader, CompletionCredentialHeader, "Authorization", "Proxy-Authorization", DefaultRegistrationTokenHeader} {
				if response.Header.Get(name) != "" || response.Trailer.Get(name) != "" {
					t.Errorf("client received sensitive %s", name)
				}
			}
			if response.Trailer.Get("X-Result") != "safe-trailer" {
				t.Fatal("safe trailer lost")
			}
			if len(stateStore.finished) != 0 {
				t.Fatal("HTTP acceptance self-completed work")
			}
		})
	}
}

func TestAdapterCompletionSeparatesAuthorityAndPreservesRaces(t *testing.T) {
	ctx := context.Background()
	stateStore, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()
	state, err := stateStore.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state.DesiredWorkload, state.ActiveWorkload = control.WorkloadMedia, control.WorkloadMedia
	state.Phase, state.Health, state.Admission = control.PhaseStable, control.HealthHealthy, control.AdmissionOpen
	state, err = stateStore.UpdateState(ctx, state.Version, state)
	if err != nil {
		t.Fatal(err)
	}
	var token string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token = r.Header.Get(DefaultRegistrationTokenHeader)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer upstream.Close()
	handler := adapterHandler(t, stateStore, upstream.URL)
	admit := func() {
		request := httptest.NewRequest("POST", "/execute", nil)
		request.Header.Set(ExecutionCredentialHeader, executionCredential)
		request.Header.Set(DefaultRequestIDHeader, "reused")
		request.Header.Set(DefaultFenceIDHeader, state.LeaseFence.Incarnation)
		request.Header.Set(DefaultFenceEpochHeader, strconv.FormatUint(state.LeaseFence.Epoch, 10))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusAccepted || response.Header().Get(DefaultRegistrationTokenHeader) != "" {
			t.Fatalf("admit: %d", response.Code)
		}
	}
	finish := func(credential, registration string, outcome store.WorkOutcome) int {
		body, err := json.Marshal(finishRequest{"reused", registration, state.LeaseFence, outcome})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest("POST", DefaultCompletionPath, strings.NewReader(string(body)))
		if credential != "" {
			request.Header.Set(CompletionCredentialHeader, credential)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response.Code
	}
	admit()
	oldToken := token
	if finish("", token, store.WorkCompleted) != http.StatusUnauthorized || finish(executionCredential, token, store.WorkAbandoned) != http.StatusUnauthorized {
		t.Fatal("execution client self-asserted completion")
	}
	if finish(completionCredential, "forged", store.WorkCompleted) != http.StatusConflict {
		t.Fatal("completion credential bypassed correlation")
	}
	var wg sync.WaitGroup
	statuses := make(chan int, 2)
	for _, outcome := range []store.WorkOutcome{store.WorkCompleted, store.WorkAbandoned} {
		wg.Add(1)
		go func(outcome store.WorkOutcome) {
			defer wg.Done()
			statuses <- finish(completionCredential, oldToken, outcome)
		}(outcome)
	}
	wg.Wait()
	close(statuses)
	for status := range statuses {
		if status != http.StatusNoContent && status != http.StatusConflict {
			t.Fatalf("race status %d", status)
		}
	}
	if n, err := stateStore.PruneCompletedWork(ctx, time.Now().Add(time.Hour), 256); err != nil || n != 1 {
		t.Fatalf("prune: %d %v", n, err)
	}
	admit()
	if token == oldToken || finish(completionCredential, oldToken, store.WorkCompleted) != http.StatusConflict {
		t.Fatal("old tuple finished reused ID")
	}
	if finish(completionCredential, token, store.WorkCompleted) != http.StatusNoContent || finish(completionCredential, token, store.WorkCompleted) != http.StatusNoContent {
		t.Fatal("trusted completion retry failed")
	}
}

func TestAdapterFiltersInformationalResponsesAndRequestTrailers(t *testing.T) {
	sensitive := []string{ExecutionCredentialHeader, CompletionCredentialHeader, "Authorization", "Proxy-Authorization", DefaultRegistrationTokenHeader}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			t.Error(err)
		}
		for _, name := range sensitive {
			if r.Trailer.Get(name) != "" {
				t.Errorf("upstream received sensitive trailer %s", name)
			}
			w.Header().Set(name, "informational-secret")
		}
		if r.Trailer.Get("X-Result") != "safe-input" {
			t.Error("safe request trailer lost")
		}
		w.Header().Set("Link", "</safe>; rel=preload")
		w.WriteHeader(http.StatusEarlyHints)
		for _, name := range sensitive {
			w.Header().Del(name)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer upstream.Close()
	proxy := httptest.NewServer(adapterHandler(t, &fakeStore{state: admittedState(control.OwnerSupervisor)}, upstream.URL))
	defer proxy.Close()
	request, _ := http.NewRequest("PATCH", proxy.URL+"/edit", strings.NewReader("payload"))
	request.ContentLength = -1
	request.Header.Set(ExecutionCredentialHeader, executionCredential)
	request.Trailer = make(http.Header)
	for _, name := range sensitive {
		request.Trailer.Set(name, "client-trailer-secret")
	}
	request.Trailer.Set("X-Result", "safe-input")
	hints := 0
	trace := &httptrace.ClientTrace{Got1xxResponse: func(code int, header textproto.MIMEHeader) error {
		hints++
		if code != http.StatusEarlyHints || header.Get("Link") == "" {
			t.Error("safe informational response lost")
		}
		for _, name := range sensitive {
			if header.Get(name) != "" {
				t.Errorf("informational response leaked %s", name)
			}
		}
		return nil
	}}
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), trace))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusAccepted || hints != 1 {
		t.Fatalf("response %d, hints %d", response.StatusCode, hints)
	}
}

func TestAdapterNativeCompletionAndUserForwarding(t *testing.T) {
	for _, owner := range []control.Owner{control.OwnerSupervisor, control.OwnerUser} {
		for _, terminal := range []bool{false, true} {
			body := `{"choices":[{"finish_reason":null}]}`
			if terminal {
				body = `{"choices":[{"finish_reason":"stop"}]}`
			}
			fixture := newTerminalFixture(t, "vllm", "/v1/completions", "application/json", body)
			fixture.store.state.Owner = owner
			target, _ := url.Parse(fixture.upstream.URL)
			config := adapterConfig(target)
			config.ExecutionRoutes = []Route{{"POST", "/v1/completions"}}
			config.ReadOnlyRoutes, config.PassthroughRoutes = nil, nil
			handler, err := NewWithContext(context.Background(), fixture.store, config)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest("POST", "/v1/completions", strings.NewReader(`{"model":"selected"}`))
			addLeaseHeaders(request, fixture.store.state.LeaseFence)
			request.Header.Set(ExecutionCredentialHeader, executionCredential)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			drainNative(t, handler)
			if response.Code != http.StatusOK || response.Header().Get(DefaultRegistrationTokenHeader) != "" {
				t.Fatalf("native execution: %d", response.Code)
			}
			wantFinished := terminal && owner == control.OwnerSupervisor
			if (len(fixture.store.finished) == 1) != wantFinished {
				t.Fatal("adapter mode changed native completion semantics")
			}
			callback := httptest.NewRequest("POST", DefaultCompletionPath, unreadableBody{t})
			response = httptest.NewRecorder()
			handler.ServeHTTP(response, callback)
			if response.Code != http.StatusUnauthorized {
				t.Fatal("native callback bypassed authentication")
			}
		}
	}
}
