package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

func observeAll(d terminalDetector, chunks ...string) bool {
	terminal := false
	for _, chunk := range chunks {
		terminal = d.observe([]byte(chunk), false)
	}
	return d.observe(nil, true) || terminal
}

func TestOllamaStreamTerminalEvidence(t *testing.T) {
	cases := map[string]struct {
		chunks   []string
		terminal bool
	}{
		"stream done":               {[]string{"{\"response\":\"a\",\"done\":false}\n", "{\"response\":\"b\",\"done\":false}\n", "{\"done\":true,\"done_reason\":\"stop\"}\n"}, true},
		"non-stream single object":  {[]string{"{\"response\":\"ab\",\"done\":true}"}, true},
		"truncated stream":          {[]string{"{\"response\":\"a\",\"done\":false}\n"}, false},
		"done false at eof":         {[]string{"{\"response\":\"a\",\"done\":false}"}, false},
		"invalid line kills trust":  {[]string{"not json\n", "{\"done\":true}\n"}, false},
		"duplicate key kills trust": {[]string{"{\"done\":false,\"done\":true}\n"}, false},
		"done wrong type":           {[]string{"{\"done\":\"true\"}"}, false},
		"blank lines ignored":       {[]string{"\n{\"done\":false}\n\n", "{\"done\":true}\n"}, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := observeAll(&ollamaStreamDetector{}, tc.chunks...); got != tc.terminal {
				t.Fatalf("terminal = %v, want %v", got, tc.terminal)
			}
		})
	}
}

func TestSSEDoneTerminalEvidence(t *testing.T) {
	cases := map[string]struct {
		chunks   []string
		terminal bool
	}{
		"done event":              {[]string{"data: {\"choices\":[]}\n\n", "data: [DONE]\n\n"}, true},
		"done split across reads": {[]string{"data: [DO", "NE]\n"}, true},
		"eof without done":        {[]string{"data: {\"choices\":[]}\n\n"}, false},
		"bare marker not data":    {[]string{"[DONE]\n"}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := observeAll(&sseDoneDetector{}, tc.chunks...); got != tc.terminal {
				t.Fatalf("terminal = %v, want %v", got, tc.terminal)
			}
		})
	}
}

func TestSynchronousBodyTerminalEvidence(t *testing.T) {
	cases := map[string]struct {
		detector terminalDetector
		body     string
		terminal bool
	}{
		"embeddings object":       {&objectKeyDetector{key: "embeddings"}, `{"model":"m","embeddings":[[0.1]]}`, true},
		"legacy embedding object": {&objectKeyDetector{key: "embedding"}, `{"embedding":[0.1]}`, true},
		"v1 embeddings object":    {&objectKeyDetector{key: "data"}, `{"data":[{"embedding":[0.1]}]}`, true},
		"result key missing":      {&objectKeyDetector{key: "embeddings"}, `{"model":"m"}`, false},
		"invalid json":            {&objectKeyDetector{key: "data"}, `{`, false},
		"duplicate key rejected":  {&objectKeyDetector{key: "data"}, `{"data":[],"data":[]}`, false},
		"choices finished":        {&completionChoicesDetector{}, `{"choices":[{"finish_reason":"stop"}]}`, true},
		"choices unfinished":      {&completionChoicesDetector{}, `{"choices":[{"finish_reason":null}]}`, false},
		"choices empty":           {&completionChoicesDetector{}, `{"choices":[]}`, false},
		"choices missing":         {&completionChoicesDetector{}, `{"id":"x"}`, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := observeAll(tc.detector, tc.body); got != tc.terminal {
				t.Fatalf("terminal = %v, want %v", got, tc.terminal)
			}
		})
	}
}

func TestDetectorSelectionByRouteAndContentType(t *testing.T) {
	cases := map[string]struct {
		path        string
		contentType string
		want        terminalDetector
	}{
		"ollama generate":     {"/api/generate", "application/x-ndjson", &ollamaStreamDetector{}},
		"ollama chat":         {"/api/chat", "application/json", &ollamaStreamDetector{}},
		"ollama embed":        {"/api/embed", "application/json", &objectKeyDetector{key: "embeddings"}},
		"ollama legacy embed": {"/api/embeddings", "application/json", &objectKeyDetector{key: "embedding"}},
		"v1 stream":           {"/v1/chat/completions", "text/event-stream; charset=utf-8", &sseDoneDetector{}},
		"v1 non-stream":       {"/v1/chat/completions", "application/json", &completionChoicesDetector{}},
		"v1 completions":      {"/v1/completions", "application/json", &completionChoicesDetector{}},
		"v1 embeddings":       {"/v1/embeddings", "application/json", &objectKeyDetector{key: "data"}},
		"unclassified route":  {"/v1/models", "application/json", nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := newTerminalDetector(tc.path, tc.contentType)
			if fmt.Sprintf("%T", got) != fmt.Sprintf("%T", tc.want) {
				t.Fatalf("detector = %T, want %T", got, tc.want)
			}
		})
	}
}

func TestOversizedResponseNeverTerminates(t *testing.T) {
	d := &ollamaStreamDetector{}
	terminal := false
	chunk := strings.Repeat("x", 1<<20)
	for range maxObservedResponseBytes/(1<<20) + 1 {
		terminal = d.observe([]byte(chunk), false)
	}
	if terminal || d.observe([]byte("{\"done\":true}\n"), true) {
		t.Fatal("oversized response terminated")
	}
}

type terminalFixture struct {
	store    *nativeStore
	handler  *Handler
	upstream *httptest.Server
}

func newTerminalFixture(t *testing.T, runtime, route, contentType, body string) *terminalFixture {
	t.Helper()
	return newTerminalFixtureWith(t, runtime, route, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		fmt.Fprint(w, body)
	}))
}

func newTerminalFixtureWith(t *testing.T, runtime, route string, upstreamHandler http.Handler) *terminalFixture {
	t.Helper()
	upstream := httptest.NewServer(upstreamHandler)
	t.Cleanup(upstream.Close)
	u, _ := url.Parse(upstream.URL)
	s := &nativeStore{fakeStore: fakeStore{state: admittedState(control.OwnerSupervisor)}, catalog: control.CatalogSnapshot{Revision: "one", Catalog: control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "media", NativeModel: &control.NativeModel{Runtime: runtime, Model: "selected", Endpoint: upstream.URL}}}}}}
	h, err := NewWithContext(context.Background(), s, Config{Upstream: u, Workload: "media", ExecutionRoutes: []Route{{Method: "POST", Path: route}}})
	if err != nil {
		t.Fatal(err)
	}
	return &terminalFixture{store: s, handler: h, upstream: upstream}
}

func (f *terminalFixture) execute(t *testing.T, route, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", route, strings.NewReader(body))
	req.Header.Set(DefaultRequestIDHeader, "request-1")
	addLeaseHeaders(req, f.store.state.LeaseFence)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, req)
	return w
}

func TestNativeExecutionCompletesOnTerminalEvidence(t *testing.T) {
	cases := map[string]struct {
		runtime     string
		route       string
		contentType string
		body        string
		finished    bool
	}{
		"ollama stream done":       {"ollama", "/api/generate", "application/x-ndjson", "{\"done\":false}\n{\"done\":true}\n", true},
		"ollama truncated stream":  {"ollama", "/api/generate", "application/x-ndjson", "{\"done\":false}\n", false},
		"ollama embed":             {"ollama", "/api/embed", "application/json", `{"embeddings":[[0.1]]}`, true},
		"llama stream done":        {"llama.cpp", "/v1/chat/completions", "text/event-stream", "data: {}\n\ndata: [DONE]\n\n", true},
		"llama stream eof":         {"llama.cpp", "/v1/chat/completions", "text/event-stream", "data: {}\n\n", false},
		"vllm non-stream finished": {"vllm", "/v1/chat/completions", "application/json", `{"choices":[{"finish_reason":"stop"}]}`, true},
		"vllm non-stream partial":  {"vllm", "/v1/chat/completions", "application/json", `{"choices":[{"finish_reason":null}]}`, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newTerminalFixture(t, tc.runtime, tc.route, tc.contentType, tc.body)
			w := f.execute(t, tc.route, `{"model":"selected"}`)
			if w.Code != 200 {
				t.Fatalf("status = %d", w.Code)
			}
			finished := len(f.store.finished) == 1 && f.store.outcomes[0] == store.WorkCompleted
			if finished != tc.finished {
				t.Fatalf("finished = %v, want %v (finished=%#v)", finished, tc.finished, f.store.finished)
			}
		})
	}
}

func TestNativeTerminalObservationSkipsUnsafeResponses(t *testing.T) {
	f := newTerminalFixture(t, "ollama", "/api/generate", "application/json", "{\"done\":true}")
	f.store.state.Owner = control.OwnerUser
	f.store.state.ActiveWorkload, f.store.state.DesiredWorkload = "media", "media"
	w := f.execute(t, "/api/generate", `{"model":"selected"}`)
	if w.Code != 200 || len(f.store.admitted) != 0 || len(f.store.finished) != 0 {
		t.Fatal("user-mode execution registered or finished work")
	}
}

func TestNativeTerminalObservationSkipsEncodedAndFailedResponses(t *testing.T) {
	cases := map[string]struct {
		status        int
		contentEncode string
	}{
		"upstream error":      {500, ""},
		"encoded body opaque": {200, "br"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newTerminalFixtureWith(t, "ollama", "/api/generate", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.contentEncode != "" {
					w.Header().Set("Content-Encoding", tc.contentEncode)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, "{\"done\":true}")
			}))
			w := f.execute(t, "/api/generate", `{"model":"selected"}`)
			if w.Code != tc.status || len(f.store.finished) != 0 {
				t.Fatalf("unsafe response finished work: %d %#v", w.Code, f.store.finished)
			}
		})
	}
}

func TestNativeTerminalFinishFailureLeavesResponseIntact(t *testing.T) {
	f := newTerminalFixture(t, "ollama", "/api/generate", "application/x-ndjson", "{\"done\":true}\n")
	f.store.finishErr = store.ErrAdmissionClosed
	w := f.execute(t, "/api/generate", `{"model":"selected"}`)
	if w.Code != 200 || w.Body.String() != "{\"done\":true}\n" {
		t.Fatalf("response damaged by finish failure: %d %q", w.Code, w.Body.String())
	}
	if len(f.store.finished) != 0 {
		t.Fatal("failed finish recorded")
	}
}
