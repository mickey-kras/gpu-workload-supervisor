package modelcheck

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// scriptedHub answers the refs request, then the tree requests with the
// statuses of script in order. After the script, it answers with the tree.
type scriptedHub struct {
	server     *httptest.Server
	mu         sync.Mutex
	script     []int
	retryAfter string
	trees      int
	auth       []string
}

func newScriptedHub(t *testing.T, script ...int) *scriptedHub {
	t.Helper()
	h := &scriptedHub{script: script}
	h.server = httptest.NewTLSServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.server.Close)
	return h
}

func (h *scriptedHub) serve(w http.ResponseWriter, r *http.Request) {
	base := "/api/models/" + testSource.Repo
	h.mu.Lock()
	defer h.mu.Unlock()
	h.auth = append(h.auth, r.Header.Get("Authorization"))
	switch r.URL.Path {
	case base + "/refs":
		raw, _ := json.Marshal(map[string][]hubRef{"branches": {{Name: "main", TargetCommit: testSource.Revision}}, "tags": {}})
		_, _ = w.Write(raw)
	case base + "/tree/" + testSource.Revision:
		h.trees++
		if len(h.script) > 0 {
			status := h.script[0]
			h.script = h.script[1:]
			if h.retryAfter != "" {
				w.Header().Set("Retry-After", h.retryAfter)
			}
			w.WriteHeader(status)
			return
		}
		raw, _ := json.Marshal([]UpstreamFile{{Type: "file", Path: "config.json", OID: strings.Repeat("1", 40), Size: 2}})
		_, _ = w.Write(raw)
	default:
		http.NotFound(w, r)
	}
}

// waits records each retry wait and does not wait.
type waits struct {
	got []time.Duration
	err error
}

func (w *waits) wait(_ context.Context, d time.Duration) error {
	w.got = append(w.got, d)
	return w.err
}

// list runs one listing with a retry that does not wait and has no jitter:
// each backoff wait is its limit.
func (h *scriptedHub) list(w *waits) (map[string]UpstreamFile, error) {
	options := hubOptions{retry: hubRetry{wait: w.wait, jitter: func(d time.Duration) time.Duration { return d }}}
	return hubListing(h.server.Client(), h.server.URL, options)(context.Background(), testSource)
}

func TestHubRetriesRateLimitThenPasses(t *testing.T) {
	h := newScriptedHub(t, http.StatusTooManyRequests)
	var w waits
	files, err := h.list(&w)
	if err != nil || len(files) != 1 || h.trees != 2 {
		t.Fatalf("listing = %v, %v after %d tree requests; want a pass on the retry", files, err, h.trees)
	}
	if len(w.got) != 1 || w.got[0] != defaultRetryBase {
		t.Fatalf("waits = %v; want one backoff of %v", w.got, defaultRetryBase)
	}
}

func TestHubHonorsRetryAfter(t *testing.T) {
	for name, tc := range map[string]struct {
		header string
		want   time.Duration
	}{
		"seconds": {"7", 7 * time.Second},
		"zero":    {"0", 0},
	} {
		t.Run(name, func(t *testing.T) {
			h := newScriptedHub(t, http.StatusServiceUnavailable)
			h.retryAfter = tc.header
			var w waits
			if _, err := h.list(&w); err != nil {
				t.Fatal(err)
			}
			if len(w.got) != 1 || w.got[0] != tc.want {
				t.Fatalf("waits = %v; want %v from Retry-After", w.got, tc.want)
			}
		})
	}
}

func TestHubGivesUpOnLongRetryAfter(t *testing.T) {
	h := newScriptedHub(t, http.StatusTooManyRequests)
	h.retryAfter = "3600"
	var w waits
	_, err := h.list(&w)
	var upstream upstreamError
	if !errors.As(err, &upstream) || upstream.reason != ReasonUpstreamUnreachable || h.trees != 1 || len(w.got) != 0 {
		t.Fatalf("listing = %v after %d tree requests and waits %v; want no retry", err, h.trees, w.got)
	}
}

func TestHubRetriesAreBounded(t *testing.T) {
	h := newScriptedHub(t, http.StatusBadGateway, http.StatusBadGateway, http.StatusBadGateway, http.StatusBadGateway, http.StatusBadGateway)
	var w waits
	_, err := h.list(&w)
	var upstream upstreamError
	if !errors.As(err, &upstream) || upstream.reason != ReasonUpstreamUnreachable || h.trees != defaultRetryAttempts {
		t.Fatalf("listing = %v after %d tree requests; want unreachable after %d", err, h.trees, defaultRetryAttempts)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	if len(w.got) != len(want) || w.got[0] != want[0] || w.got[1] != want[1] || w.got[2] != want[2] {
		t.Fatalf("waits = %v; want doubling backoff %v", w.got, want)
	}
}

func TestHubDoesNotRetryDefiniteFailures(t *testing.T) {
	h := newScriptedHub(t, http.StatusNotFound)
	var w waits
	if _, err := h.list(&w); err == nil || h.trees != 1 || len(w.got) != 0 {
		t.Fatalf("listing = %v after %d tree requests; want no retry of not found", err, h.trees)
	}
}

func TestHubStopsRetryWhenTheWaitEnds(t *testing.T) {
	h := newScriptedHub(t, http.StatusServiceUnavailable, http.StatusServiceUnavailable)
	w := waits{err: context.Canceled}
	_, err := h.list(&w)
	var upstream upstreamError
	if !errors.As(err, &upstream) || upstream.reason != ReasonUpstreamUnreachable || h.trees != 1 {
		t.Fatalf("listing = %v after %d tree requests; want a stop after the canceled wait", err, h.trees)
	}
}

// A retry sends the token again, and the error holds only a reason code.
func TestHubRetryKeepsTokenOutOfErrors(t *testing.T) {
	const token = "hf_secretretrytoken"
	t.Setenv("HF_TOKEN", token)
	h := newScriptedHub(t, http.StatusTooManyRequests, http.StatusTooManyRequests, http.StatusTooManyRequests, http.StatusTooManyRequests)
	var w waits
	_, err := h.list(&w)
	if err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("listing error = %v; want a failure without the token", err)
	}
	for _, header := range h.auth {
		if header != "Bearer "+token {
			t.Fatalf("request without the token: %q", header)
		}
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for value, want := range map[string]struct {
		d      time.Duration
		hinted bool
	}{
		"":               {0, false},
		"-1":             {0, false},
		"soon":           {0, false},
		"12":             {12 * time.Second, true},
		"99999999999999": {maxRetryAfter + time.Second, true},
		now.Add(30 * time.Second).Format(http.TimeFormat): {30 * time.Second, true},
		now.Add(-time.Hour).Format(http.TimeFormat):       {0, true},
	} {
		response := &http.Response{Header: http.Header{}}
		if value != "" {
			response.Header.Set("Retry-After", value)
		}
		d, hinted := retryAfter(response, now)
		if d != want.d || hinted != want.hinted {
			t.Errorf("Retry-After %q = %v, %v; want %v, %v", value, d, hinted, want.d, want.hinted)
		}
	}
}

func TestRetryDefaultsAndJitter(t *testing.T) {
	r := hubOptions{}.withDefaults().retry
	if r.attempts != defaultRetryAttempts || r.base != defaultRetryBase || r.max != defaultRetryMax {
		t.Fatalf("defaults = %+v", r)
	}
	for retry := 1; retry <= 10; retry++ {
		if d, ok := r.delay(retry, 0, false); !ok || d < 0 || d > defaultRetryMax {
			t.Fatalf("delay %d = %v, %v; want a jittered wait within the cap", retry, d, ok)
		}
	}
	if fullJitter(0) != 0 {
		t.Fatal("jitter of zero must be zero")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitContext(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait on a canceled context = %v", err)
	}
	if err := waitContext(context.Background(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
}
