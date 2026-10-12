package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/httptransport"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

type comfyUIFakeStore struct {
	attempts           []store.ComfyUIAttempt
	finished           []store.ComfyUIAttempt
	finishErr, listErr error
}

func TestComfyUIRealGzipEvidenceRemainsUnresolved(t *testing.T) {
	a := comfyUIFixtureAttempt()
	for _, compressedRoute := range []string{"history", "queue"} {
		t.Run(compressedRoute, func(t *testing.T) {
			runtime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Accept-Encoding") != "identity" {
					t.Error("runtime evidence requested compression")
				}
				raw := comfyUIFixtureHistory(a)
				route := "history"
				if r.URL.Path == "/queue" {
					raw = `{"queue_running":[],"queue_pending":[]}`
					route = "queue"
				}
				if route != compressedRoute {
					io.WriteString(w, raw)
					return
				}
				w.Header().Set("Content-Encoding", "gzip")
				encoded := gzip.NewWriter(w)
				if _, err := io.WriteString(encoded, raw); err != nil {
					t.Error(err)
				}
				if err := encoded.Close(); err != nil {
					t.Error(err)
				}
			}))
			defer runtime.Close()
			u, _ := url.Parse(runtime.URL)
			s := &comfyUIFakeStore{attempts: []store.ComfyUIAttempt{a}}
			adapter := &comfyUIAdapter{store: s, upstream: u, transport: httptransport.NewDirect(), workload: a.Workload}
			err := adapter.reconcile(context.Background())
			var unresolved *ComfyUIUnresolvedError
			if !errors.As(err, &unresolved) || len(s.finished) != 0 {
				t.Fatalf("compressed evidence accepted: %v completions=%d", err, len(s.finished))
			}
		})
	}
}

func TestComfyUIAlreadyDecompressedEvidenceRemainsUnresolved(t *testing.T) {
	a := comfyUIFixtureAttempt()
	u, _ := url.Parse("http://127.0.0.1:8188")
	s := &comfyUIFakeStore{attempts: []store.ComfyUIAttempt{a}}
	adapter := &comfyUIAdapter{store: s, upstream: u, workload: a.Workload, transport: comfyUIRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Uncompressed: true, Body: io.NopCloser(strings.NewReader(comfyUIFixtureHistory(a)))}, nil
	})}
	if err := adapter.reconcile(context.Background()); err == nil || len(s.finished) != 0 {
		t.Fatalf("decoded compressed evidence accepted: %v", err)
	}
}

func (s *comfyUIFakeStore) AdmitComfyUIWork(context.Context, store.ComfyUIAttempt, string) (string, error) {
	return "", nil
}
func (s *comfyUIFakeStore) PendingComfyUIAttempts(_ context.Context, _ control.Workload, _, _, after string, _ int) ([]store.ComfyUIAttempt, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	var result []store.ComfyUIAttempt
	for _, a := range s.attempts {
		if a.AttemptID > after {
			result = append(result, a)
		}
	}
	return result, nil
}
func (s *comfyUIFakeStore) FinishWorkToken(_ context.Context, id string, w control.Workload, f control.Fence, token string, _ store.WorkOutcome) error {
	s.finished = append(s.finished, store.ComfyUIAttempt{RequestID: id, Workload: w, Fence: f, RegistrationToken: token})
	return s.finishErr
}
func comfyUIFixtureAttempt() store.ComfyUIAttempt {
	return store.ComfyUIAttempt{AttemptID: "11111111-1111-4111-8111-111111111111", PromptID: "22222222-2222-4222-8222-222222222222", RequestID: "request", RegistrationToken: "private-token", Workload: control.WorkloadMedia, Fence: control.Fence{Incarnation: "original", Epoch: 1}}
}
func comfyUIFixtureHistory(a store.ComfyUIAttempt) string {
	raw, err := os.ReadFile("testdata/comfyui/history-success.json")
	if err != nil {
		panic(err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		panic(err)
	}
	result := strings.ReplaceAll(compact.String(), "22222222-2222-4222-8222-222222222222", a.PromptID)
	return strings.ReplaceAll(result, "11111111-1111-4111-8111-111111111111", a.AttemptID)
}

func TestComfyUIPromptCorrelationReplacesCallerIdentityWithoutStoringGraph(t *testing.T) {
	body, attempt, prompt, err := prepareComfyUIPrompt(strings.NewReader(`{"prompt":{"1":{"class_type":"FixtureOutput","inputs":{"value":"private"}}},"prompt_id":"caller","extra_data":{"workflow":{"private":true},"gpu_workload_supervisor":{"attemptId":"forged"}}}`))
	if err != nil || attempt == "" || prompt == "" || attempt == prompt {
		t.Fatalf("%s %s %v", attempt, prompt, err)
	}
	var result map[string]json.RawMessage
	json.Unmarshal(body, &result)
	if string(result["prompt_id"]) != `"`+prompt+`"` || !strings.Contains(string(body), "private") || strings.Contains(string(body), "forged") {
		t.Fatal("correlation or graph changed incorrectly")
	}
	for _, bad := range []string{`null`, `[]`, `{"prompt":{}}`, `{"prompt":{},"prompt":{}}`, `{"prompt":{"1":{}},"extra_data":null}`, `{"prompt":{"1":{}},"extra_data":{"GPU_workload_supervisor":{}}}`, `{"prompt":{"1":{}}} {}`} {
		if _, _, _, err := prepareComfyUIPrompt(strings.NewReader(bad)); err == nil {
			t.Fatalf("invalid envelope accepted %s", bad)
		}
	}
}

func TestComfyUICompletionRequiresCorrelatedTerminalAndQueueAbsence(t *testing.T) {
	a := comfyUIFixtureAttempt()
	valid := comfyUIFixtureHistory(a)
	for _, tc := range []struct {
		name, history, queue string
		complete             bool
	}{
		{"success", valid, `{"queue_running":[],"queue_pending":[]}`, true},
		{"acceptance-only", `{}`, `{"queue_running":[],"queue_pending":[]}`, false},
		{"wrong-attempt", strings.ReplaceAll(valid, a.AttemptID, "foreign"), `{"queue_running":[],"queue_pending":[]}`, false},
		{"wrong-prompt", strings.ReplaceAll(valid, a.PromptID, "foreign"), `{"queue_running":[],"queue_pending":[]}`, false},
		{"not-completed", strings.ReplaceAll(valid, `"completed":true`, `"completed":false`), `{"queue_running":[],"queue_pending":[]}`, false},
		{"runtime-error", strings.ReplaceAll(valid, `"success"`, `"error"`), `{"queue_running":[],"queue_pending":[]}`, false},
		{"status-alias", strings.ReplaceAll(valid, `"status":`, `"Status":`), `{"queue_running":[],"queue_pending":[]}`, false},
		{"completed-alias", strings.ReplaceAll(valid, `"completed":true`, `"Completed":true`), `{"queue_running":[],"queue_pending":[]}`, false},
		{"completed-conflict", strings.ReplaceAll(valid, `"completed":true`, `"completed":true,"Completed":false`), `{"queue_running":[],"queue_pending":[]}`, false},
		{"status-str-alias", strings.ReplaceAll(valid, `"status_str":`, `"Status_str":`), `{"queue_running":[],"queue_pending":[]}`, false},
		{"prompt-alias", strings.ReplaceAll(valid, `"prompt":`, `"Prompt":`), `{"queue_running":[],"queue_pending":[]}`, false},
		{"attempt-alias", strings.ReplaceAll(valid, `"attemptId":`, `"AttemptId":`), `{"queue_running":[],"queue_pending":[]}`, false},
		{"attempt-conflict", strings.ReplaceAll(valid, `"attemptId":`, `"AttemptId":"foreign","attemptId":`), `{"queue_running":[],"queue_pending":[]}`, false},
		{"metadata-alias", strings.ReplaceAll(valid, `"gpu_workload_supervisor":`, `"GPU_workload_supervisor":`), `{"queue_running":[],"queue_pending":[]}`, false},
		{"null-status", strings.ReplaceAll(valid, `"status":{`, `"status":null,"ignored":{`), `{"queue_running":[],"queue_pending":[]}`, false},
		{"null-prompt", strings.ReplaceAll(valid, `"prompt":[`, `"prompt":null,"ignored":[`), `{"queue_running":[],"queue_pending":[]}`, false},
		{"null-tuple-number", strings.ReplaceAll(valid, `"prompt":[1,`, `"prompt":[null,`), `{"queue_running":[],"queue_pending":[]}`, false},
		{"bad-tuple-number", strings.ReplaceAll(valid, `"prompt":[1,`, `"prompt":["1",`), `{"queue_running":[],"queue_pending":[]}`, false},
		{"pending", valid, `{"queue_running":[],"queue_pending":[[1,"` + a.PromptID + `",{}, {},[]]]}`, false},
		{"running", valid, `{"queue_running":[[1,"` + a.PromptID + `",{}, {},[]]],"queue_pending":[]}`, false},
		{"missing-queue", valid, `{"queue_running":[]}`, false},
		{"null-queue", valid, `{"queue_running":null,"queue_pending":[]}`, false},
		{"malformed-queue", valid, `{"queue_running":[[1]],"queue_pending":[]}`, false},
		{"invalid-queue-type", valid, `{"queue_running":{},"queue_pending":[]}`, false},
		{"invalid-foreign-tuple", valid, `{"queue_running":[[null,"33333333-3333-4333-8333-333333333333",{}, {},[]]],"queue_pending":[]}`, false},
		{"invalid-foreign-output", valid, `{"queue_running":[[1,"33333333-3333-4333-8333-333333333333",{}, {},null]],"queue_pending":[]}`, false},
		{"queue-conflicting-alias", valid, `{"queue_running":[],"Queue_running":[[1,"foreign",{}, {},[]]],"queue_pending":[]}`, false},
		{"duplicate-history", `{"x":{},"x":{}}`, `{"queue_running":[],"queue_pending":[]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					posts++
					t.Error("recovery dispatched execution")
				}
				if strings.HasPrefix(r.URL.Path, "/history/") {
					w.Write([]byte(tc.history))
				} else {
					w.Write([]byte(tc.queue))
				}
			}))
			defer server.Close()
			upstream, _ := url.Parse(server.URL)
			s := &comfyUIFakeStore{attempts: []store.ComfyUIAttempt{a}}
			adapter := &comfyUIAdapter{store: s, upstream: upstream, transport: http.DefaultTransport, workload: a.Workload}
			err := adapter.reconcile(context.Background())
			if (len(s.finished) == 1) != tc.complete || posts != 0 {
				t.Fatalf("premature/lost completion: %v %v", s.finished, err)
			}
			if !tc.complete {
				var unresolved *ComfyUIUnresolvedError
				if !errors.As(err, &unresolved) {
					t.Fatalf("untyped uncertainty %v", err)
				}
				if strings.Contains(err.Error(), a.RegistrationToken) {
					t.Fatal("token leaked")
				}
			}
			if tc.complete && (s.finished[0].Fence != a.Fence || s.finished[0].RegistrationToken != a.RegistrationToken) {
				t.Fatal("lost admitted tuple")
			}
			// Cursor wraps without ever re-enqueueing an ambiguous attempt.
			if err := adapter.reconcile(context.Background()); err != nil || adapter.cursor != "" {
				t.Fatal("cursor did not wrap")
			}
		})
	}
}

type comfyUIBrokenReader struct{}

func (comfyUIBrokenReader) Read([]byte) (int, error) { return 0, errors.New("fixture read failure") }

type comfyUIRoundTripFunc func(*http.Request) (*http.Response, error)

func (f comfyUIRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestComfyUIBoundsAndTransportUncertainty(t *testing.T) {
	for _, r := range []io.Reader{comfyUIBrokenReader{}, strings.NewReader(strings.Repeat(" ", comfyUIBodyLimit+1))} {
		if _, _, _, err := prepareComfyUIPrompt(r); err == nil {
			t.Fatal("unbounded/unreadable request accepted")
		}
	}
	base := `{"prompt":{"1":{"inputs":{"value":""}}}}`
	large := strings.Replace(base, `"value":""`, `"value":"`+strings.Repeat("x", comfyUIBodyLimit-len(base))+`"`, 1)
	if _, _, _, err := prepareComfyUIPrompt(strings.NewReader(large)); err == nil {
		t.Fatal("correlation expansion exceeds limit")
	}
	upstream, _ := url.Parse("http://127.0.0.1:8188")
	a := comfyUIFixtureAttempt()
	for _, mode := range []string{"transport", "read", "oversize", "invalid-url", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			adapter := &comfyUIAdapter{store: &comfyUIFakeStore{attempts: []store.ComfyUIAttempt{a}}, upstream: upstream, workload: a.Workload}
			adapter.transport = comfyUIRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				if mode == "transport" {
					return nil, errors.New("fixture")
				}
				var body io.ReadCloser = io.NopCloser(strings.NewReader(strings.Repeat("x", comfyUIBodyLimit+1)))
				if mode == "read" {
					body = io.NopCloser(comfyUIBrokenReader{})
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body}, nil
			})
			if mode == "invalid-url" {
				adapter.upstream = &url.URL{Scheme: ":"}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancelled" {
				cancel()
				if err := adapter.reconcile(ctx); !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled pass: %v", err)
				}
				return
			}
			if err := adapter.reconcile(ctx); err == nil {
				t.Fatal("uncertainty accepted")
			}
		})
	}
}

func TestComfyUIRuntimeFailuresAndCompletionFailureRemainUnresolved(t *testing.T) {
	a := comfyUIFixtureAttempt()
	for _, mode := range []string{"history-error", "queue-error", "redirect", "compressed", "truncated", "completion-error", "missing-store"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "redirect" {
					w.Header().Set("Location", "/prompt")
					w.WriteHeader(302)
					return
				}
				if mode == "compressed" {
					w.Header().Set("Content-Encoding", "gzip")
				}
				if mode == "history-error" || mode == "queue-error" && r.URL.Path == "/queue" {
					w.WriteHeader(503)
					return
				}
				if mode == "truncated" {
					w.Write([]byte(`{"unfinished":`))
					return
				}
				if r.URL.Path == "/queue" {
					w.Write([]byte(`{"queue_running":[],"queue_pending":[]}`))
					return
				}
				w.Write([]byte(comfyUIFixtureHistory(a)))
			}))
			defer server.Close()
			upstream, _ := url.Parse(server.URL)
			s := &comfyUIFakeStore{attempts: []store.ComfyUIAttempt{a}}
			if mode == "completion-error" {
				s.finishErr = errors.New("fixture")
			}
			if mode == "missing-store" {
				s.listErr = errors.New("fixture")
			}
			adapter := &comfyUIAdapter{store: s, upstream: upstream, transport: http.DefaultTransport, workload: a.Workload}
			if err := adapter.reconcile(context.Background()); err == nil {
				t.Fatal("uncertainty accepted")
			}
		})
	}
}
