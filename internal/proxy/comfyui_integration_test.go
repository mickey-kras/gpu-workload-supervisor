package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

func comfyUIConfig(upstream *url.URL) Config {
	return Config{Upstream: upstream, Workload: control.WorkloadMedia, ExecutionRoutes: []Route{{Method: "POST", Path: "/prompt"}}, IngressMode: IngressAdapter, ExecutionCredential: executionCredential, CompletionCredential: completionCredential, ComfyUIContract: ComfyUIContract}
}

func TestComfyUIConstructorRejectsUnqualifiedDeployment(t *testing.T) {
	f := newContractFixture(t)
	target, _ := url.Parse("http://127.0.0.1:8188")
	for _, mode := range []string{"future-contract", "trusted", "query", "empty-query", "fragment", "base-path", "escaped-path", "extra-execution", "passthrough", "job-header", "unsafe-monitor", "wrong-method", "missing-store"} {
		t.Run(mode, func(t *testing.T) {
			config := comfyUIConfig(target)
			u := *target
			config.Upstream = &u
			var stateStore StateStore = f.store
			switch mode {
			case "future-contract":
				config.ComfyUIContract = "future"
			case "trusted":
				config.IngressMode = IngressTrusted
				config.ExecutionCredential = ""
				config.CompletionCredential = ""
			case "query":
				u.RawQuery = "x=1"
			case "empty-query":
				u.ForceQuery = true
			case "fragment":
				u.Fragment = "x"
			case "base-path":
				u.Path = "/api"
			case "escaped-path":
				u.RawPath = "/%61pi"
			case "extra-execution":
				config.ExecutionRoutes = append(config.ExecutionRoutes, Route{Method: "POST", Path: "/api/jobs"})
			case "passthrough":
				config.PassthroughRoutes = []Route{{Method: "POST", Path: "/interrupt"}}
			case "job-header":
				config.JobIDHeader = "X-Job-ID"
			case "unsafe-monitor":
				config.ReadOnlyRoutes = []Route{{Method: "GET", Path: "/ws"}}
			case "wrong-method":
				config.ReadOnlyRoutes = []Route{{Method: "HEAD", Path: "/queue"}}
			case "missing-store":
				stateStore = &fakeStore{}
			}
			if _, err := NewWithContext(context.Background(), stateStore, config); err == nil {
				t.Fatalf("unqualified %s accepted", mode)
			}
		})
	}
}

func TestComfyUIEnqueuePersistsBeforeDispatchAndResumesWithoutReplay(t *testing.T) {
	for _, lostReply := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "lost-reply"}[lostReply], func(t *testing.T) {
			ctx := context.Background()
			f := newContractFixture(t)
			var posts atomic.Int64
			terminal := false
			var runtimeMu sync.Mutex
			var admitted store.ComfyUIAttempt
			runtime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				runtimeMu.Lock()
				defer runtimeMu.Unlock()
				if r.Method == "POST" {
					posts.Add(1)
					list, err := f.store.PendingComfyUIAttempts(ctx, control.WorkloadMedia, "http://"+r.Host, ComfyUIContract, "", 64)
					if err != nil || len(list) != 1 {
						t.Errorf("dispatch before durable metadata: %v %v", list, err)
						w.WriteHeader(500)
						return
					}
					admitted = list[0]
					var body map[string]json.RawMessage
					raw, _ := io.ReadAll(r.Body)
					json.Unmarshal(raw, &body)
					if string(body["prompt_id"]) != `"`+admitted.PromptID+`"` || !strings.Contains(string(raw), admitted.AttemptID) {
						t.Error("runtime correlation differs")
					}
					for _, key := range []string{DefaultRegistrationTokenHeader, DefaultRequestIDHeader, ExecutionCredentialHeader, CompletionCredentialHeader, "Idempotency-Key"} {
						if r.Header.Get(key) != "" {
							t.Errorf("private/replay header reached runtime: %s", key)
						}
					}
					if lostReply {
						connection, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						connection.Close()
						return
					}
					w.Header().Set(DefaultRegistrationTokenHeader, "runtime-forgery")
					w.Write([]byte(`{"prompt_id":"` + admitted.PromptID + `"}`))
					return
				}
				if strings.HasPrefix(r.URL.Path, "/history/") {
					if terminal {
						w.Write([]byte(comfyUIFixtureHistory(admitted)))
					} else {
						w.Write([]byte(`{}`))
					}
					return
				}
				w.Write([]byte(`{"queue_running":[],"queue_pending":[]}`))
			}))
			defer runtime.Close()
			upstream, _ := url.Parse(runtime.URL)
			config := comfyUIConfig(upstream)
			config.ReadOnlyRoutes = []Route{{Method: "GET", Path: "/queue"}, {Method: "GET", Path: "/prompt"}, {Method: "GET", Path: "/history"}}
			h, err := NewWithContext(ctx, f.store, config)
			if err != nil {
				t.Fatal(err)
			}
			submit := func() *httptest.ResponseRecorder {
				r := f.request(ctx, runtime.URL, " request ")
				r.URL.Path = "/prompt"
				r.Body = io.NopCloser(strings.NewReader(`{"prompt":{"1":{"class_type":"FixtureOutput","inputs":{}}}}`))
				r.ContentLength = -1
				r.Header.Set(ExecutionCredentialHeader, executionCredential)
				r.Header.Set("Idempotency-Key", "retry-forbidden")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				return w
			}
			response := submit()
			if response.Header().Get(DefaultRegistrationTokenHeader) != "" || posts.Load() != 1 {
				t.Fatal("leaked capability/lost dispatch")
			}
			if lostReply && response.Code != 502 || !lostReply && response.Code != 200 {
				t.Fatalf("dispatch response %d", response.Code)
			}
			if err := h.comfyUI.reconcile(ctx); err == nil {
				t.Fatal("acceptance/transport failure completed work")
			}
			if count, _ := f.store.PendingWork(ctx); count != 1 {
				t.Fatal("early completion")
			}
			if response := submit(); response.Code != 409 || posts.Load() != 1 {
				t.Fatal("ambiguous enqueue replayed")
			}
			// A new process sees the original tuple and only polls backend state.
			runtimeMu.Lock()
			f.reopen()
			runtimeMu.Unlock()
			h, err = NewWithContext(ctx, f.store, config)
			if err != nil {
				t.Fatal(err)
			}
			runtimeMu.Lock()
			terminal = true
			runtimeMu.Unlock()
			if err := h.comfyUI.reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			if count, _ := f.store.PendingWork(ctx); count != 0 || posts.Load() != 1 {
				t.Fatal("restart lost terminal correlation or replayed enqueue")
			}
		})
	}
}

func TestComfyUIInvalidBodyAndWorkerCancellation(t *testing.T) {
	f := newContractFixture(t)
	target, _ := url.Parse("http://127.0.0.1:1")
	h, err := NewWithContext(context.Background(), f.store, comfyUIConfig(target))
	if err != nil {
		t.Fatal(err)
	}
	r := f.request(context.Background(), target.String(), "invalid")
	r.URL.Path = "/prompt"
	r.Body = io.NopCloser(strings.NewReader(`null`))
	r.Header.Set(ExecutionCredentialHeader, executionCredential)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatalf("body %d", w.Code)
	}
	if count, _ := f.store.PendingWork(context.Background()); count != 0 {
		t.Fatal("invalid body admitted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() { h.RunComfyUIReconciler(ctx, func(error) {}); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not cancel")
	}
}

func TestComfyUIUserExecutionRewritesIdentityUnderGateWithoutRegistration(t *testing.T) {
	f := newContractFixture(t)
	ctx := context.Background()
	state := f.state
	state.Owner = control.OwnerUser
	state.Admission = control.AdmissionClosed
	if _, err := f.store.UpdateState(ctx, state.Version, state); err != nil {
		t.Fatal(err)
	}
	var posts atomic.Int64
	runtime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), "foreign-identity") {
			t.Error("manual caller reused supervisor correlation")
		}
		w.Write([]byte(`{"accepted":true}`))
	}))
	defer runtime.Close()
	u, _ := url.Parse(runtime.URL)
	h, err := NewWithContext(ctx, f.store, comfyUIConfig(u))
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"prompt":{"1":{}},"prompt_id":"foreign-identity","extra_data":{"gpu_workload_supervisor":{"attemptId":"foreign-identity"}}}`, `null`} {
		r := httptest.NewRequest("POST", "/prompt", strings.NewReader(body))
		r.Header.Set(ExecutionCredentialHeader, executionCredential)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if body == `null` && w.Code != 400 || body != `null` && w.Code != 200 {
			t.Fatalf("user response %d", w.Code)
		}
	}
	if posts.Load() != 1 {
		t.Fatal("invalid manual envelope reached runtime")
	}
	if count, _ := f.store.PendingWork(ctx); count != 0 {
		t.Fatal("manual work registered")
	}
}

func TestComfyUIWorkerReportsUnresolvedAttemptAndCancels(t *testing.T) {
	u, _ := url.Parse("http://127.0.0.1:8188")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := &Handler{comfyUI: &comfyUIAdapter{upstream: u, store: &comfyUIFakeStore{attempts: []store.ComfyUIAttempt{comfyUIFixtureAttempt()}}, transport: comfyUIRoundTripFunc(func(*http.Request) (*http.Response, error) { return nil, io.ErrUnexpectedEOF })}}
	reported := false
	h.RunComfyUIReconciler(ctx, func(err error) {
		if !strings.Contains(err.Error(), "history_unavailable") {
			t.Errorf("missing typed uncertainty: %v", err)
		}
		reported = true
		cancel()
	})
	if !reported {
		t.Fatal("unresolved attempt not reported")
	}
}
