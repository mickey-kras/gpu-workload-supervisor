package proxy

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

// The upstream reads and accepts execution, then loses its response on a reused
// connection. A transport retry would execute the same admission twice.
func TestExecutionDoesNotReplayAfterLostResponse(t *testing.T) {
	for _, mode := range []string{"trusted", "adapter", "native"} {
		for _, owner := range []control.Owner{control.OwnerSupervisor, control.OwnerUser} {
			for _, input := range []struct{ method, body, hint string }{
				{"POST", "", "Idempotency-Key"},
				{"POST", "", "X-Idempotency-Key"},
				{"POST", "{}", "Idempotency-Key"},
				{"GET", "", ""}, {"HEAD", "", ""}, {"OPTIONS", "", ""}, {"TRACE", "", ""},
			} {
				if mode == "native" && input.method != "POST" {
					continue
				}
				t.Run(fmt.Sprintf("%s/%s/%s/%s/%s", mode, owner, input.method, input.body, input.hint), func(t *testing.T) {
					path, monitor, body := "/execute", "/monitor", input.body
					if mode == "native" {
						path, monitor, body = "/v1/completions", "/v1/models", `{"model":"selected"}`
					}
					var calls atomic.Int32
					var warmAddress string
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						got, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
						}
						if r.URL.Path == monitor {
							if warmAddress != "" && r.RemoteAddr != warmAddress {
								t.Error("read-only connection was not reused")
							}
							warmAddress = r.RemoteAddr
							w.WriteHeader(http.StatusNoContent)
							return
						}
						if body == "" && (len(r.TransferEncoding) != 0 || r.ContentLength != 0) {
							t.Errorf("empty execution framing: transfer encoding=%v content length=%d", r.TransferEncoding, r.ContentLength)
						}
						if string(got) != body {
							t.Errorf("upstream body = %q, want %q", got, body)
						}
						if calls.Add(1) == 1 {
							if r.RemoteAddr != warmAddress {
								t.Error("execution did not use warmed connection")
							}
							c, _, err := w.(http.Hijacker).Hijack()
							if err != nil {
								t.Error(err)
								return
							}
							_ = c.Close()
							return
						}
						w.WriteHeader(http.StatusAccepted)
					}))
					defer upstream.Close()
					target, _ := url.Parse(upstream.URL)
					state := admittedState(owner)
					baseStore := &fakeStore{state: state}
					var stateStore StateStore = baseStore
					config := Config{Upstream: target, Workload: control.WorkloadMedia, ExecutionRoutes: []Route{{Method: input.method, Path: path}}, ReadOnlyRoutes: []Route{{Method: "GET", Path: monitor}}}
					switch mode {
					case "adapter":
						config.IngressMode, config.ExecutionCredential, config.CompletionCredential = IngressAdapter, executionCredential, completionCredential
					case "native":
						stateStore = &nativeStore{fakeStore: fakeStore{state: state}, catalog: control.CatalogSnapshot{Revision: "one", Catalog: control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "media", NativeModel: &control.NativeModel{Runtime: "vllm", Model: "selected", Endpoint: upstream.URL, APIKeyFile: proxyKeyFile(t)}}}}}}
						baseStore = &stateStore.(*nativeStore).fakeStore
					}
					h, err := NewWithContext(context.Background(), stateStore, config)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { drainNative(t, h) })
					for i := 0; i < 2; i++ {
						r := httptest.NewRequest("GET", monitor, nil)
						if config.IngressMode == IngressAdapter {
							r.Header.Set(ExecutionCredentialHeader, executionCredential)
						}
						w := httptest.NewRecorder()
						h.ServeHTTP(w, r)
						if w.Code != http.StatusNoContent {
							t.Fatalf("warm status=%d", w.Code)
						}
					}
					// NewRequest supplies GetBody for in-process callers with a nonempty body.
					r, err := http.NewRequest(input.method, "http://proxy.test"+path, strings.NewReader(body))
					if err != nil {
						t.Fatal(err)
					}
					addLeaseHeaders(r, state.LeaseFence)
					if input.hint != "" {
						r.Header.Set(input.hint, "retry-is-unsafe")
					}
					if config.IngressMode == IngressAdapter {
						r.Header.Set(ExecutionCredentialHeader, executionCredential)
					}
					w := httptest.NewRecorder()
					h.ServeHTTP(w, r)
					if n := calls.Load(); n != 1 {
						t.Errorf("upstream executions=%d, want 1", n)
					}
					if w.Code != http.StatusBadGateway {
						t.Errorf("response status=%d, want 502", w.Code)
					}
					if owner == control.OwnerSupervisor && len(baseStore.admitted) != 1 {
						t.Errorf("admissions=%v, want one", baseStore.admitted)
					}
					if len(baseStore.finished) != 0 {
						t.Errorf("ambiguous execution completed: %v", baseStore.finished)
					}
				})
			}
		}
	}
}

func TestRequesterExecutionDoesNotReplayAfterLostResponse(t *testing.T) {
	var calls atomic.Int32
	var address string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			address = r.RemoteAddr
		}
		if n == 2 {
			if r.RemoteAddr != address {
				t.Error("requester execution did not reuse connection")
			}
			c, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = c.Close()
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer upstream.Close()
	config := requesterConfig()
	config.Upstream, _ = url.Parse(upstream.URL)
	config.RequesterPolicy.Requesters[0].Workflows[0].BodySHA256 = fmt.Sprintf("%x", sha256.Sum256(nil))
	stateStore := &requesterStore{fakeStore: fakeStore{state: admittedState(control.OwnerSupervisor)}}
	h, err := NewWithContext(context.Background(), stateStore, config)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest("POST", "/execute", nil)
		r.Header.Set(RequesterCredentialHeader, requesterCredential)
		r.Header.Set(WorkflowHeader, "approved")
		r.Header.Set(DefaultRequestIDHeader, fmt.Sprintf("request-%d", i))
		r.Header.Set("Idempotency-Key", "retry-is-unsafe")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		want := http.StatusAccepted
		if i == 1 {
			want = http.StatusBadGateway
		}
		if w.Code != want {
			t.Errorf("response %d status=%d, want %d", i, w.Code, want)
		}
	}
	if calls.Load() != 2 || len(stateStore.admitted) != 2 || len(stateStore.finished) != 0 {
		t.Fatalf("executions=%d admissions=%v finished=%v", calls.Load(), stateStore.admitted, stateStore.finished)
	}
}

func TestExecutionPreservesCorrelationUsingReplayHeaderName(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Idempotency-Key"); got != "request-1" {
			t.Errorf("registered request ID=%q", got)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	stateStore := &fakeStore{state: admittedState(control.OwnerSupervisor)}
	h, err := NewWithContext(context.Background(), stateStore, Config{Upstream: target, Workload: control.WorkloadMedia, ExecutionRoutes: []Route{{Method: "POST", Path: "/execute"}}, RequestIDHeader: "Idempotency-Key"})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/execute", nil)
	addLeaseHeaders(r, stateStore.state.LeaseFence)
	r.Header.Set("Idempotency-Key", "request-1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("response status=%d", w.Code)
	}
}

func TestEmptyExecutionOverHTTP2(t *testing.T) {
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("upstream protocol=%s", r.Proto)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || len(body) != 0 {
			t.Errorf("empty body=%q error=%v", body, err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	stateStore := &fakeStore{state: admittedState(control.OwnerSupervisor)}
	h, err := NewWithContext(context.Background(), stateStore, Config{Upstream: target, Workload: control.WorkloadMedia, ExecutionRoutes: []Route{{Method: "POST", Path: "/execute"}}, Transport: upstream.Client().Transport})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/execute", nil)
	addLeaseHeaders(r, stateStore.state.LeaseFence)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("response status=%d", w.Code)
	}
}
