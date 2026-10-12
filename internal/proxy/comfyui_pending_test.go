package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/httptransport"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

func TestComfyUICorrelatedPendingPollsAreQuietAndRemainUnfinished(t *testing.T) {
	for _, queueKey := range []string{"queue_pending", "queue_running"} {
		t.Run(queueKey, func(t *testing.T) {
			ctx := context.Background()
			f := newContractFixture(t)
			attempt := comfyUIFixtureAttempt()
			attempt.Fence = f.state.LeaseFence
			var historyReads, queueReads atomic.Int64
			runtime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/queue" {
					historyReads.Add(1)
					w.Write([]byte(`{}`))
					return
				}
				queueReads.Add(1)
				queue := map[string][][]json.RawMessage{"queue_running": {}, "queue_pending": {}}
				var history map[string]json.RawMessage
				json.Unmarshal([]byte(comfyUIFixtureHistory(attempt)), &history)
				var entry map[string]json.RawMessage
				json.Unmarshal(history[attempt.PromptID], &entry)
				var tuple []json.RawMessage
				json.Unmarshal(entry["prompt"], &tuple)
				queue[queueKey] = append(queue[queueKey], tuple)
				json.NewEncoder(w).Encode(queue)
			}))
			defer runtime.Close()
			upstream, _ := url.Parse(runtime.URL)
			attempt.Upstream = runtime.URL
			attempt.Contract = ComfyUIContract
			if _, err := f.store.AdmitComfyUIWork(ctx, attempt, ""); err != nil {
				t.Fatal(err)
			}
			h, err := NewWithContext(ctx, f.store, comfyUIConfig(upstream))
			if err != nil {
				t.Fatal(err)
			}
			for poll := 0; poll < 6; poll++ {
				if err := h.comfyUI.reconcile(ctx); err != nil {
					t.Fatalf("normal pending poll reported operational error: %v", err)
				}
			}
			if pending, err := f.store.PendingWork(ctx); err != nil || pending != 1 {
				t.Fatalf("pending work falsely completed: %d %v", pending, err)
			}
			if historyReads.Load() != 3 || queueReads.Load() != 3 {
				t.Fatalf("poll fetch budget changed: history=%d queue=%d", historyReads.Load(), queueReads.Load())
			}
		})
	}
}

func TestComfyUIUncertainPendingEvidenceStillReported(t *testing.T) {
	a := comfyUIFixtureAttempt()
	tuple := `[1,"` + a.PromptID + `",{}, {"gpu_workload_supervisor":{"attemptId":"` + a.AttemptID + `"}},[]]`
	validQueue := `{"queue_running":[],"queue_pending":[` + tuple + `]}`
	errorHistory := strings.ReplaceAll(strings.ReplaceAll(comfyUIFixtureHistory(a), `"completed":true`, `"completed":false`), `"success"`, `"error"`)
	for _, tc := range []struct {
		name, history, queue string
		queueStatus          int
	}{
		{"missing-from-backend", `{}`, `{"queue_running":[],"queue_pending":[]}`, 200},
		{"foreign-attempt", `{}`, strings.ReplaceAll(validQueue, a.AttemptID, "foreign"), 200},
		{"missing-correlation", `{}`, `{"queue_running":[],"queue_pending":[[1,"` + a.PromptID + `",{}, {},[]]]}`, 200},
		{"duplicate-presence", `{}`, `{"queue_running":[` + tuple + `],"queue_pending":[` + tuple + `]}`, 200},
		{"malformed-queue", `{}`, `{"queue_running":null,"queue_pending":[]}`, 200},
		{"null-history", `null`, validQueue, 200},
		{"runtime-error", errorHistory, validQueue, 200},
		{"queue-unavailable", `{}`, validQueue, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/queue" {
					w.WriteHeader(tc.queueStatus)
					w.Write([]byte(tc.queue))
					return
				}
				w.Write([]byte(tc.history))
			}))
			defer runtime.Close()
			u, _ := url.Parse(runtime.URL)
			s := &comfyUIFakeStore{attempts: []store.ComfyUIAttempt{a}}
			adapter := &comfyUIAdapter{store: s, upstream: u, transport: httptransport.NewDirect(), workload: a.Workload}
			var unresolved *ComfyUIUnresolvedError
			if err := adapter.reconcile(context.Background()); !errors.As(err, &unresolved) || len(s.finished) != 0 {
				t.Fatalf("uncertainty hidden/completed: %v", err)
			}
		})
	}
}
