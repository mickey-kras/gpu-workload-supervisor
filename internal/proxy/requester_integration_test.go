package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

func requesterIntegrationServer(t *testing.T, f *contractFixture, upstream string) *httptest.Server {
	t.Helper()
	c := requesterConfig()
	var err error
	c.Upstream, err = url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewWithContext(context.Background(), f.store, c)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	return server
}

func requesterIntegrationRequest(t *testing.T, origin, method, id string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(method, origin+"/execute", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set(RequesterCredentialHeader, requesterCredential)
	r.Header.Set(WorkflowHeader, "approved")
	r.Header.Set(DefaultRequestIDHeader, id)
	return r
}

func TestRequesterDurableAdmissionRestartAndTrustedCompletion(t *testing.T) {
	for _, interrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "truncated stream"}[interrupt], func(t *testing.T) {
			f := newContractFixture(t)
			var dispatched atomic.Int32
			correlation := make(chan finishRequest, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				dispatched.Add(1)
				id, token := r.Header.Get(DefaultRequestIDHeader), r.Header.Get(DefaultRegistrationTokenHeader)
				f.assertWork(id, token, false)
				var requester, workflow, digest, status, workload string
				if err := f.rows.QueryRow(`SELECT requester,workflow,body_sha256,status,workload FROM broker_demands WHERE request_id=?`, id).Scan(&requester, &workflow, &digest, &status, &workload); err != nil {
					t.Error(err)
				}
				if requester != "automation" || workflow != "approved" || digest != requesterConfig().RequesterPolicy.Requesters[0].Workflows[0].BodySHA256 || status != "admitted" || workload != "media" {
					t.Errorf("unbound admission: %s %s %s %s", requester, workflow, status, workload)
				}
				if r.Header.Get(RequesterCredentialHeader) != "" || r.Header.Get(WorkflowHeader) != "" {
					t.Error("requester authorization leaked upstream")
				}
				correlation <- finishRequest{RequestID: id, RegistrationToken: token, Fence: f.state.LeaseFence, Outcome: store.WorkCompleted}
				if interrupt {
					w.Header().Set("Content-Length", "100")
					w.Header().Set("Content-Type", "text/event-stream")
				}
				w.WriteHeader(http.StatusAccepted)
				_, _ = io.WriteString(w, "partial")
				w.(http.Flusher).Flush()
			}))
			defer upstream.Close()
			server := requesterIntegrationServer(t, f, upstream.URL)
			response, err := server.Client().Do(requesterIntegrationRequest(t, server.URL, http.MethodPost, "request-1"))
			if err != nil {
				t.Fatal(err)
			}
			_, readErr := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != http.StatusAccepted || interrupt && readErr == nil || !interrupt && readErr != nil {
				t.Fatalf("response=%d read=%v", response.StatusCode, readErr)
			}
			finish := <-correlation
			f.assertWork("request-1", finish.RegistrationToken, false)
			server.Close()
			f.reopen()
			server = requesterIntegrationServer(t, f, upstream.URL)
			duplicate, err := server.Client().Do(requesterIntegrationRequest(t, server.URL, http.MethodPost, "request-1"))
			if err != nil {
				t.Fatal(err)
			}
			duplicate.Body.Close()
			if duplicate.StatusCode != http.StatusConflict || dispatched.Load() != 1 {
				t.Fatalf("restart redispatch status=%d count=%d", duplicate.StatusCode, dispatched.Load())
			}
			body, err := json.Marshal(finish)
			if err != nil {
				t.Fatal(err)
			}
			for _, credential := range []string{requesterCredential, completionCredential} {
				r, err := http.NewRequest(http.MethodPost, server.URL+DefaultCompletionPath, strings.NewReader(string(body)))
				if err != nil {
					t.Fatal(err)
				}
				r.Header.Set(CompletionCredentialHeader, credential)
				completed, err := server.Client().Do(r)
				if err != nil {
					t.Fatal(err)
				}
				completed.Body.Close()
				expected := http.StatusUnauthorized
				if credential == completionCredential {
					expected = http.StatusNoContent
				}
				if completed.StatusCode != expected {
					t.Fatalf("completion=%d want=%d", completed.StatusCode, expected)
				}
				f.assertWork("request-1", finish.RegistrationToken, credential == completionCredential)
			}
		})
	}
}

func TestRequesterQueuedDemandCancellation(t *testing.T) {
	f := newContractFixture(t)
	state := f.state
	state.ActiveWorkload = control.WorkloadText
	state.DesiredWorkload = control.WorkloadText
	var err error
	f.state, err = f.store.UpdateState(context.Background(), state.Version, state)
	if err != nil {
		t.Fatal(err)
	}
	server := requesterIntegrationServer(t, f, "http://127.0.0.1:1")
	for _, id := range []string{"first", "second"} {
		response, err := server.Client().Do(requesterIntegrationRequest(t, server.URL, http.MethodPost, id))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusConflict {
			t.Fatalf("busy queue=%d", response.StatusCode)
		}
	}
	response, err := server.Client().Do(requesterIntegrationRequest(t, server.URL, http.MethodDelete, "first"))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("cancel=%d", response.StatusCode)
	}
	var queued, cancelled, work int
	if err := f.rows.QueryRow(`SELECT SUM(status='queued'),SUM(status='cancelled') FROM broker_demands`).Scan(&queued, &cancelled); err != nil {
		t.Fatal(err)
	}
	if err := f.rows.QueryRow(`SELECT COUNT(*) FROM registered_work`).Scan(&work); err != nil {
		t.Fatal(err)
	}
	if queued != 1 || cancelled != 1 || work != 0 {
		t.Fatalf("queued=%d cancelled=%d dispatched=%d", queued, cancelled, work)
	}
}
