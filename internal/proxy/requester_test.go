package proxy

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

const requesterCredential = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

func requesterConfig() Config {
	target, _ := url.Parse("http://127.0.0.1:1")
	c := adapterConfig(target)
	c.ReadOnlyRoutes, c.PassthroughRoutes = nil, nil
	c.RequesterPolicy = &RequesterPolicy{Requesters: []Requester{{ID: "automation", Credential: requesterCredential, Workflows: []Workflow{{ID: "approved", Method: "POST", Path: "/execute", BodySHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(`{}`)))}}}}}
	c.ActivateDemand = func(context.Context, control.Workload, control.State, string) (control.Fence, error) {
		return control.Fence{}, errors.New("deferred")
	}
	return c
}

func requesterRequest() *http.Request {
	r := httptest.NewRequest("POST", "/execute", strings.NewReader(`{}`))
	r.Header.Set(RequesterCredentialHeader, requesterCredential)
	r.Header.Set(WorkflowHeader, "approved")
	r.Header.Set(DefaultRequestIDHeader, "request-1")
	return r
}

func TestRequesterAuthorizationRejectsBeforeQueue(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*http.Request)
		status int
	}{
		{"missing credential", func(r *http.Request) { r.Header.Del(RequesterCredentialHeader) }, 401},
		{"bad credential", func(r *http.Request) { r.Header.Set(RequesterCredentialHeader, executionCredential) }, 401},
		{"duplicate credential", func(r *http.Request) { r.Header.Add(RequesterCredentialHeader, requesterCredential) }, 401},
		{"execution credential", func(r *http.Request) { r.Header.Set(ExecutionCredentialHeader, executionCredential) }, 401},
		{"completion credential", func(r *http.Request) { r.Header.Set(CompletionCredentialHeader, completionCredential) }, 401},
		{"missing workflow", func(r *http.Request) { r.Header.Del(WorkflowHeader) }, 403},
		{"unknown workflow", func(r *http.Request) { r.Header.Set(WorkflowHeader, "other") }, 403},
		{"method", func(r *http.Request) { r.Method = "GET" }, 403},
		{"route", func(r *http.Request) { r.URL.Path = "/monitor" }, 403},
		{"force query", func(r *http.Request) { r.URL.ForceQuery = true }, 403},
		{"raw path", func(r *http.Request) { r.URL.RawPath = "/%65xecute" }, 403},
		{"upgrade", func(r *http.Request) { r.Header.Set("Upgrade", "websocket") }, 403},
		{"query", func(r *http.Request) { r.URL.RawQuery = "override=1" }, 403},
		{"encoding", func(r *http.Request) { r.Header.Set("Content-Encoding", "gzip") }, 403},
		{"trailers", func(r *http.Request) { r.Trailer = http.Header{"Other": nil} }, 403},
		{"changed body", func(r *http.Request) { r.Body = http.NoBody }, 403},
		{"body error", func(r *http.Request) { r.Body = badRequesterBody{} }, 400},
		{"missing request", func(r *http.Request) { r.Header.Del(DefaultRequestIDHeader) }, 400},
		{"duplicate request", func(r *http.Request) { r.Header.Add(DefaultRequestIDHeader, "two") }, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &requesterStore{fakeStore: fakeStore{state: admittedState(control.OwnerSupervisor)}}
			h, err := NewWithContext(context.Background(), s, requesterConfig())
			if err != nil {
				t.Fatal(err)
			}
			r := requesterRequest()
			tc.change(r)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status || s.queued != 0 || len(s.admitted) != 0 {
				t.Fatalf("status %d queue %d admission %v", w.Code, s.queued, s.admitted)
			}
		})
	}
}

type badRequesterBody struct{}

func (badRequesterBody) Read([]byte) (int, error) { return 0, errors.New("read failed") }
func (badRequesterBody) Close() error             { return nil }

type requesterStore struct {
	fakeStore
	queued, reserved, requeued                 int
	queueErr, reserveErr, requeueErr, stateErr error
}

func (s *requesterStore) State(ctx context.Context) (control.State, error) {
	if s.stateErr != nil {
		return control.State{}, s.stateErr
	}
	return s.fakeStore.State(ctx)
}
func (s *requesterStore) QueueDemand(context.Context, store.Demand) error {
	s.queued++
	return s.queueErr
}
func (s *requesterStore) ReserveDemand(context.Context, store.Demand) (string, error) {
	s.reserved++
	return "claim", s.reserveErr
}
func (s *requesterStore) RequeueDemand(context.Context, string, string) error {
	s.requeued++
	return s.requeueErr
}

func TestRequesterDemandDispatchAndDeferral(t *testing.T) {
	for _, tc := range []struct {
		name                               string
		change                             func(*requesterStore, *Config)
		status, queued, reserved, requeued int
	}{
		{"running", func(*requesterStore, *Config) {}, 202, 1, 1, 0},
		{"user", func(s *requesterStore, _ *Config) { s.state.Owner = control.OwnerUser }, 409, 0, 0, 0},
		{"error", func(s *requesterStore, _ *Config) { s.state.Health = control.HealthError }, 409, 0, 0, 0},
		{"state failure", func(s *requesterStore, _ *Config) { s.stateErr = errors.New("db") }, 503, 0, 0, 0},
		{"mixed", func(s *requesterStore, _ *Config) { s.state.ActiveWorkload = control.WorkloadText }, 409, 1, 0, 0},
		{"transition", func(s *requesterStore, _ *Config) { s.state.Phase = control.PhaseDraining }, 409, 1, 0, 0},
		{"queue conflict", func(s *requesterStore, _ *Config) { s.queueErr = store.ErrDemandConflict }, 409, 1, 0, 0},
		{"capacity", func(s *requesterStore, _ *Config) { s.queueErr = store.ErrDemandCapacity }, 503, 1, 0, 0},
		{"queue unavailable", func(s *requesterStore, _ *Config) { s.queueErr = errors.New("db") }, 503, 1, 0, 0},
		{"reserved", func(s *requesterStore, _ *Config) { s.reserveErr = store.ErrDemandConflict }, 409, 1, 1, 0},
		{"activation deferred", func(s *requesterStore, _ *Config) { s.state.ActiveWorkload = control.WorkloadIdle }, 409, 1, 1, 1},
		{"requeue unavailable", func(s *requesterStore, _ *Config) {
			s.state.ActiveWorkload = control.WorkloadIdle
			s.requeueErr = errors.New("db")
		}, 503, 1, 1, 1},
		{"cold activation", func(s *requesterStore, c *Config) {
			s.state.ActiveWorkload = control.WorkloadIdle
			c.ActivateDemand = func(context.Context, control.Workload, control.State, string) (control.Fence, error) {
				s.state = admittedState(control.OwnerSupervisor)
				return s.state.LeaseFence, nil
			}
		}, 202, 1, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &requesterStore{fakeStore: fakeStore{state: admittedState(control.OwnerSupervisor)}}
			c := requesterConfig()
			tc.change(s, &c)
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get(RequesterCredentialHeader) != "" || r.Header.Get(WorkflowHeader) != "" {
					t.Error("requester metadata leaked")
				}
				w.WriteHeader(202)
			}))
			defer upstream.Close()
			c.Upstream, _ = url.Parse(upstream.URL)
			h, err := NewWithContext(context.Background(), s, c)
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, requesterRequest())
			if w.Code != tc.status || s.queued != tc.queued || s.reserved != tc.reserved || s.requeued != tc.requeued {
				t.Fatalf("response %d q/r/r %d/%d/%d: %s", w.Code, s.queued, s.reserved, s.requeued, w.Body.String())
			}
			if tc.status != 202 && calls != 0 {
				t.Fatal("rejected demand reached runtime")
			}
		})
	}
}
func (s *requesterStore) CancelDemand(context.Context, store.Demand) error { return nil }

func TestRequesterPolicyValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Config)
	}{
		{"trusted", func(c *Config) { c.IngressMode = IngressTrusted }},
		{"passthrough", func(c *Config) { c.PassthroughRoutes = []Route{{"POST", "/edit"}} }},
		{"read", func(c *Config) { c.ReadOnlyRoutes = []Route{{"GET", "/status"}} }},
		{"empty", func(c *Config) { c.RequesterPolicy.Requesters = nil }},
		{"too many", func(c *Config) { c.RequesterPolicy.Requesters = make([]Requester, 129) }},
		{"invalid identity", func(c *Config) { c.RequesterPolicy.Requesters[0].ID = "bad identity" }},
		{"empty identity", func(c *Config) { c.RequesterPolicy.Requesters[0].ID = "" }},
		{"duplicate", func(c *Config) {
			c.RequesterPolicy.Requesters = append(c.RequesterPolicy.Requesters, c.RequesterPolicy.Requesters[0])
		}},
		{"credential alias", func(c *Config) { c.RequesterPolicy.Requesters[0].Credential = executionCredential }},
		{"invalid credential", func(c *Config) { c.RequesterPolicy.Requesters[0].Credential = "bad" }},
		{"no workflows", func(c *Config) { c.RequesterPolicy.Requesters[0].Workflows = nil }},
		{"too many workflows", func(c *Config) { c.RequesterPolicy.Requesters[0].Workflows = make([]Workflow, 129) }},
		{"invalid workflow", func(c *Config) { c.RequesterPolicy.Requesters[0].Workflows[0].ID = "!" }},
		{"duplicate workflow", func(c *Config) {
			r := &c.RequesterPolicy.Requesters[0]
			r.Workflows = append(r.Workflows, r.Workflows[0])
		}},
		{"digest", func(c *Config) { c.RequesterPolicy.Requesters[0].Workflows[0].BodySHA256 = "bad" }},
		{"delete", func(c *Config) { c.RequesterPolicy.Requesters[0].Workflows[0].Method = "DELETE" }},
		{"route", func(c *Config) { c.RequesterPolicy.Requesters[0].Workflows[0].Path = "/other" }},
		{"header alias", func(c *Config) { c.RequestIDHeader = RequesterCredentialHeader }},
		{"workflow header alias", func(c *Config) { c.JobIDHeader = WorkflowHeader }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := requesterConfig()
			tc.change(&c)
			if _, err := NewWithContext(context.Background(), &requesterStore{}, c); err == nil {
				t.Fatal("invalid policy accepted")
			}
		})
	}
	c := requesterConfig()
	if _, err := NewWithContext(context.Background(), &fakeStore{}, c); err == nil {
		t.Fatal("missing durable store accepted")
	}
	c.ActivateDemand = nil
	if _, err := NewWithContext(context.Background(), &requesterStore{}, c); err == nil {
		t.Fatal("missing activation accepted")
	}
}

func TestRequesterPolicyCopiedAndCompletionDenied(t *testing.T) {
	c := requesterConfig()
	s := &requesterStore{fakeStore: fakeStore{state: admittedState(control.OwnerSupervisor)}}
	h, err := NewWithContext(context.Background(), s, c)
	if err != nil {
		t.Fatal(err)
	}
	c.RequesterPolicy.Requesters[0].Credential = executionCredential
	c.RequesterPolicy.Requesters[0].Workflows[0].BodySHA256 = "modified"
	w := httptest.NewRecorder()
	id, _, ok := h.authorizeRequester(w, requesterRequest())
	if !ok || id != "automation" {
		t.Fatal("caller mutated policy")
	}
	r := requesterRequest()
	r.URL.Path = DefaultCompletionPath
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 || len(s.finished) != 0 {
		t.Fatalf("requester completed work: %d", w.Code)
	}
	r = requesterRequest()
	r.Method = "DELETE"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 204 || s.queued != 0 || s.reserved != 0 {
		t.Fatalf("cancel changed admission %d", w.Code)
	}
}
