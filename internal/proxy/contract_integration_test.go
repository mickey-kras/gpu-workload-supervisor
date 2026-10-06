package proxy

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

type contractFixture struct {
	t     *testing.T
	path  string
	store *store.Store
	rows  *sql.DB
	state control.State
}

func newContractFixture(t *testing.T) *contractFixture {
	t.Helper()
	f := &contractFixture{t: t, path: filepath.Join(t.TempDir(), "state.db")}
	f.reopen()
	var err error
	f.state, err = f.store.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.state.ActiveWorkload, f.state.DesiredWorkload = control.WorkloadMedia, control.WorkloadMedia
	f.state.Health, f.state.Phase, f.state.Admission = control.HealthHealthy, control.PhaseStable, control.AdmissionOpen
	f.state, err = f.store.UpdateState(context.Background(), f.state.Version, f.state)
	if err != nil {
		t.Fatal(err)
	}
	f.rows, err = sql.Open("sqlite", "file:"+f.path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.rows.Close(); f.store.Close() })
	return f
}

func (f *contractFixture) reopen() {
	f.t.Helper()
	if f.store != nil {
		if err := f.store.Close(); err != nil {
			f.t.Fatal(err)
		}
	}
	var err error
	f.store, err = store.Open(context.Background(), f.path)
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *contractFixture) server(upstream string, stateStore StateStore) *httptest.Server {
	f.t.Helper()
	u, err := url.Parse(upstream)
	if err != nil {
		f.t.Fatal(err)
	}
	h, err := NewWithContext(context.Background(), stateStore, Config{Upstream: u, Workload: control.WorkloadMedia, ExecutionRoutes: []Route{{Method: "POST", Path: "/execute"}}})
	if err != nil {
		f.t.Fatal(err)
	}
	s := httptest.NewServer(h)
	f.t.Cleanup(s.Close)
	return s
}

func (f *contractFixture) request(ctx context.Context, origin, id string) *http.Request {
	f.t.Helper()
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+"/execute", nil)
	if err != nil {
		f.t.Fatal(err)
	}
	r.Header.Set(DefaultRequestIDHeader, id)
	r.Header.Set(DefaultFenceIDHeader, f.state.LeaseFence.Incarnation)
	r.Header.Set(DefaultFenceEpochHeader, strconv.FormatUint(f.state.LeaseFence.Epoch, 10))
	return r
}

func (f *contractFixture) assertWork(id, token string, finished bool) {
	f.t.Helper()
	var actualToken, incarnation string
	var epoch uint64
	var completed sql.NullString
	err := f.rows.QueryRow(`SELECT registration_token, lease_incarnation, lease_epoch, completed_at FROM registered_work WHERE request_id=?`, id).Scan(&actualToken, &incarnation, &epoch, &completed)
	if err != nil {
		f.t.Error(err)
		return
	}
	if actualToken != token || incarnation != f.state.LeaseFence.Incarnation || epoch != f.state.LeaseFence.Epoch || completed.Valid != finished {
		f.t.Errorf("durable correlation/completion mismatch for %s: token=%t fence=%s/%d finished=%t", id, actualToken == token, incarnation, epoch, completed.Valid)
	}
}

func (f *contractFixture) finish(origin string, finish finishRequest, want int) {
	f.t.Helper()
	body, err := json.Marshal(finish)
	if err != nil {
		f.t.Fatal(err)
	}
	response, err := http.Post(origin+DefaultCompletionPath, "application/json", strings.NewReader(string(body)))
	if err != nil {
		f.t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != want {
		data, _ := io.ReadAll(response.Body)
		f.t.Fatalf("completion status %d want %d: %s", response.StatusCode, want, data)
	}
}

func TestHTTPResponsesRequireExplicitTerminalEvidence(t *testing.T) {
	for _, status := range []int{200, 202, 400, 500} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			f := newContractFixture(t)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				f.assertWork(r.Header.Get(DefaultRequestIDHeader), r.Header.Get(DefaultRegistrationTokenHeader), false)
				w.WriteHeader(status)
				w.(http.Flusher).Flush()
				_, _ = io.WriteString(w, "stream ended")
			}))
			defer upstream.Close()
			proxy := f.server(upstream.URL, f.store)
			response, err := proxy.Client().Do(f.request(context.Background(), proxy.URL, "job"))
			if err != nil {
				t.Fatal(err)
			}
			token := response.Header.Get(DefaultRegistrationTokenHeader)
			_, err = io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || response.StatusCode != status || token == "" {
				t.Fatalf("forwarded response %d token=%t err=%v", response.StatusCode, token != "", err)
			}
			f.assertWork("job", token, false)
			finish := finishRequest{RequestID: "job", RegistrationToken: token, Fence: f.state.LeaseFence, Outcome: store.WorkCompleted}
			for _, badToken := range []string{"", "forged"} {
				forged := finish
				forged.RegistrationToken = badToken
				f.finish(proxy.URL, forged, http.StatusConflict)
				f.assertWork("job", token, false)
			}
			stale := finish
			stale.Fence.Epoch++
			f.finish(proxy.URL, stale, http.StatusConflict)
			f.finish(proxy.URL, finish, http.StatusNoContent)
			f.assertWork("job", token, true)
			finish.Outcome = store.WorkAbandoned
			f.finish(proxy.URL, finish, http.StatusNotFound)
			var outcome string
			if err := f.rows.QueryRow("SELECT completion_outcome FROM registered_work WHERE request_id='job'").Scan(&outcome); err != nil || outcome != string(store.WorkCompleted) {
				t.Fatalf("duplicate changed terminal outcome: %s %v", outcome, err)
			}
		})
	}
}

func TestInterruptedStreamsRemainUnfinishedAcrossRestart(t *testing.T) {
	for _, failure := range []string{"truncated upstream", "client disconnect", "proxy shutdown"} {
		t.Run(failure, func(t *testing.T) {
			f := newContractFixture(t)
			correlation := make(chan finishRequest, 1)
			upstreamDone := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(upstreamDone)
				correlation <- finishRequest{RequestID: r.Header.Get(DefaultRequestIDHeader), RegistrationToken: r.Header.Get(DefaultRegistrationTokenHeader), Fence: f.state.LeaseFence, Outcome: store.WorkCompleted}
				w.Header().Set("Content-Length", "100")
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "partial")
				w.(http.Flusher).Flush()
				if failure != "truncated upstream" {
					<-r.Context().Done()
				}
			}))
			defer upstream.Close()
			proxy := f.server(upstream.URL, f.store)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			response, err := proxy.Client().Do(f.request(ctx, proxy.URL, "stream"))
			if err != nil {
				t.Fatal(err)
			}
			finish := <-correlation
			if failure == "client disconnect" {
				cancel()
			}
			if failure == "proxy shutdown" {
				proxy.CloseClientConnections()
			}
			_, err = io.ReadAll(response.Body)
			response.Body.Close()
			if err == nil {
				t.Fatal("interrupted stream unexpectedly complete")
			}
			select {
			case <-upstreamDone:
			case <-time.After(3 * time.Second):
				t.Fatal("upstream not canceled")
			}
			proxy.Close()
			f.assertWork("stream", finish.RegistrationToken, false)
			f.reopen()
			f.assertWork("stream", finish.RegistrationToken, false)
			restarted := f.server(upstream.URL, f.store)
			f.finish(restarted.URL, finish, http.StatusNoContent)
			f.assertWork("stream", finish.RegistrationToken, true)
		})
	}
}

type pausedAdmission struct {
	*store.Store
	entered chan struct{}
	resume  chan struct{}
}

func (s *pausedAdmission) AdmitWorkTokenAtCatalog(ctx context.Context, id, job string, workload control.Workload, fence control.Fence, revision string) (string, error) {
	close(s.entered)
	select {
	case <-s.resume:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return s.Store.AdmitWorkTokenAtCatalog(ctx, id, job, workload, fence, revision)
}

func (f *contractFixture) beginDrain() {
	f.t.Helper()
	target := f.state
	target.DesiredWorkload = control.WorkloadIdle
	_, err := f.store.StartTransition(context.Background(), f.state.Version, store.Transition{ID: "drain", Source: f.state, Target: target, Previous: f.state, Initiator: "contract-test", Deadline: time.Now().Add(time.Minute)})
	if err != nil {
		f.t.Fatal(err)
	}
}

func TestHTTPAdmissionClosureOrdering(t *testing.T) {
	t.Run("closed before admission", func(t *testing.T) {
		f := newContractFixture(t)
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("dispatch escaped closed admission")
			w.WriteHeader(200)
		}))
		defer upstream.Close()
		paused := &pausedAdmission{Store: f.store, entered: make(chan struct{}), resume: make(chan struct{})}
		proxy := f.server(upstream.URL, paused)
		done := make(chan error, 1)
		request := f.request(context.Background(), proxy.URL, "late")
		go func() {
			response, err := proxy.Client().Do(request)
			if err == nil {
				response.Body.Close()
				if response.StatusCode != http.StatusConflict {
					err = fmt.Errorf("status %d", response.StatusCode)
				}
			}
			done <- err
		}()
		select {
		case <-paused.entered:
		case <-time.After(3 * time.Second):
			t.Fatal("admission not reached")
		}
		f.beginDrain()
		close(paused.resume)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		pending, err := f.store.PendingTransitionWork(context.Background(), "drain")
		if err != nil || pending != 0 {
			t.Fatalf("pending %d: %v", pending, err)
		}
	})
	t.Run("admitted before closure", func(t *testing.T) {
		f := newContractFixture(t)
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusAccepted) }))
		defer upstream.Close()
		proxy := f.server(upstream.URL, f.store)
		response, err := proxy.Client().Do(f.request(context.Background(), proxy.URL, "running"))
		if err != nil {
			t.Fatal(err)
		}
		token := response.Header.Get(DefaultRegistrationTokenHeader)
		response.Body.Close()
		f.beginDrain()
		pending, err := f.store.PendingTransitionWork(context.Background(), "drain")
		if err != nil || pending != 1 {
			t.Fatalf("pending %d: %v", pending, err)
		}
		f.finish(proxy.URL, finishRequest{RequestID: "running", RegistrationToken: token, Fence: f.state.LeaseFence, Outcome: store.WorkCompleted}, http.StatusNoContent)
		pending, err = f.store.PendingTransitionWork(context.Background(), "drain")
		if err != nil || pending != 0 {
			t.Fatalf("pending after terminal %d: %v", pending, err)
		}
	})
}
