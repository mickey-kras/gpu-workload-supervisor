package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

func TestComfyUIDiagnosticsPollThroughSuppressionAndPageWrap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		attempt := comfyUIFixtureAttempt()
		stateStore := &comfyUIDiagnosticStore{comfyUIFakeStore: comfyUIFakeStore{attempts: []store.ComfyUIAttempt{attempt}}}
		upstream, _ := url.Parse("http://127.0.0.1:8188")
		var polls atomic.Int64
		h := &Handler{comfyUI: &comfyUIAdapter{store: stateStore, upstream: upstream, transport: comfyUIRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			if strings.HasPrefix(r.URL.Path, "/history/") {
				polls.Add(1)
				if polls.Load() <= 2 {
					return nil, errors.New("private runtime reply")
				}
				return comfyUIDiagnosticResponse(comfyUIFixtureHistory(attempt)), nil
			}
			if polls.Load() == 3 {
				return nil, io.ErrUnexpectedEOF
			}
			return comfyUIDiagnosticResponse(`{"queue_running":[],"queue_pending":[]}`), nil
		})}}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var reports []error
		reported := make(chan error, 16)
		settle := func() {
			synctest.Wait()
			for len(reported) > 0 {
				reports = append(reports, <-reported)
			}
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			h.RunComfyUIReconciler(ctx, func(err error) { reported <- err })
		}()
		settle()
		if polls.Load() != 1 || len(reports) != 1 || reports[0].Error() != "ComfyUI attempt "+attempt.AttemptID+" unresolved: history_unavailable" {
			t.Fatalf("initial polls=%d reports=%v", polls.Load(), reports)
		}
		time.Sleep(2 * time.Second)
		settle()
		if polls.Load() != 1 || len(reports) != 1 {
			t.Fatalf("empty page claimed recovery: polls=%d reports=%v", polls.Load(), reports)
		}
		time.Sleep(2 * time.Second)
		settle()
		if polls.Load() != 2 || len(reports) != 1 || stateStore.finishedCount() != 0 {
			t.Fatalf("unchanged failure: polls=%d reports=%v finished=%v", polls.Load(), reports, stateStore.finishedCount())
		}
		time.Sleep(4 * time.Second)
		settle()
		var changed *ComfyUIUnresolvedError
		if polls.Load() != 3 || len(reports) != 2 || !errors.As(reports[1], &changed) || changed.Reason != "queue_unavailable" {
			t.Fatalf("changed failure: polls=%d reports=%v", polls.Load(), reports)
		}
		time.Sleep(4 * time.Second)
		settle()
		var recovered *ComfyUIRecovery
		if polls.Load() != 4 || stateStore.finishedCount() != 1 || len(reports) != 3 || !errors.As(reports[2], &recovered) || recovered.AttemptID != attempt.AttemptID {
			t.Fatalf("terminal detection/recovery: polls=%d reports=%v finished=%v", polls.Load(), reports, stateStore.finishedCount())
		}
		cancel()
		<-done
		time.Sleep(10 * time.Second)
		if polls.Load() != 4 || len(reports) != 3 {
			t.Fatalf("polling continued after cancellation: polls=%d reports=%v", polls.Load(), reports)
		}
	})
}

func comfyUIDiagnosticResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestComfyUIDiagnosticsRotatingFailuresAndEviction(t *testing.T) {
	d := comfyUIDiagnostics{}
	var reports []error
	report := func(err error) { reports = append(reports, err) }
	d.observe("first", "history_unavailable", report)
	d.observe("second", "queue_unavailable", report)
	for range 5 {
		d.observe("first", "history_unavailable", report)
		d.observe("second", "queue_unavailable", report)
	}
	if len(reports) != 2 {
		t.Fatalf("rotating failures repeated: %v", reports)
	}
	d.observe("first", "", report)
	d.observe("first", "", report)
	d.observe("first", "history_unavailable", report)
	if len(reports) != 4 {
		t.Fatalf("recovery/new failure not reported once: %v", reports)
	}
	for i := range comfyUIDiagnosticLimit + 1 {
		d.observe(fmt.Sprint(i), "history_unavailable", report)
	}
	before := len(reports)
	d.observe(fmt.Sprint(comfyUIDiagnosticLimit), "history_unavailable", report)
	if len(reports) != before {
		t.Fatal("recent failure was not suppressed")
	}
	d.observe("second", "queue_unavailable", report)
	if len(reports) != before+1 {
		t.Fatal("evicted failure was not reported again")
	}
}

func TestComfyUIDiagnosticsSanitizeMetadataAndRecoverWithoutAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		upstream, _ := url.Parse("http://127.0.0.1:8188")
		stateStore := &comfyUIDiagnosticStore{comfyUIFakeStore: comfyUIFakeStore{listErr: errors.New("secret-token request graph")}}
		h := &Handler{comfyUI: &comfyUIAdapter{store: stateStore, upstream: upstream}}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var reports []error
		reported := make(chan error, 16)
		settle := func() {
			synctest.Wait()
			for len(reported) > 0 {
				reports = append(reports, <-reported)
			}
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			h.RunComfyUIReconciler(ctx, func(err error) { reported <- err })
		}()
		settle()
		time.Sleep(4 * time.Second)
		settle()
		if len(reports) != 1 || reports[0].Error() != "ComfyUI reconciliation unresolved: pending_metadata_unavailable" {
			t.Fatalf("metadata diagnostics: %v", reports)
		}
		stateStore.mu.Lock()
		stateStore.listErr = nil
		stateStore.mu.Unlock()
		time.Sleep(2 * time.Second)
		settle()
		var recovered *ComfyUIRecovery
		if len(reports) != 2 || !errors.As(reports[1], &recovered) || recovered.AttemptID != "" {
			t.Fatalf("metadata recovery: %v", reports)
		}
		cancel()
		<-done
	})
}

type comfyUIDiagnosticStore struct {
	comfyUIFakeStore
	mu sync.Mutex
}

func (s *comfyUIDiagnosticStore) PendingComfyUIAttempts(ctx context.Context, w control.Workload, upstream, contract, after string, limit int) ([]store.ComfyUIAttempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.comfyUIFakeStore.PendingComfyUIAttempts(ctx, w, upstream, contract, after, limit)
}

func (s *comfyUIDiagnosticStore) FinishWorkToken(ctx context.Context, id string, w control.Workload, f control.Fence, token string, outcome store.WorkOutcome) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.comfyUIFakeStore.FinishWorkToken(ctx, id, w, f, token, outcome)
}

func (s *comfyUIDiagnosticStore) finishedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.finished)
}

func TestComfyUIDiagnosticsCancellationDuringPoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		upstream, _ := url.Parse("http://127.0.0.1:8188")
		stateStore := &comfyUIDiagnosticStore{comfyUIFakeStore: comfyUIFakeStore{attempts: []store.ComfyUIAttempt{comfyUIFixtureAttempt()}}}
		h := &Handler{comfyUI: &comfyUIAdapter{store: stateStore, upstream: upstream, transport: comfyUIRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			<-r.Context().Done()
			return nil, r.Context().Err()
		})}}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			h.RunComfyUIReconciler(ctx, func(err error) { t.Errorf("shutdown diagnostic: %v", err) })
		}()
		synctest.Wait()
		cancel()
		<-done
		if stateStore.finishedCount() != 0 {
			t.Fatal("cancellation completed work")
		}
	})
}

func TestComfyUIDiagnosticsRecoveryMayRemainPending(t *testing.T) {
	attempt := comfyUIFixtureAttempt()
	upstream, _ := url.Parse("http://127.0.0.1:8188")
	stateStore := &comfyUIFakeStore{attempts: []store.ComfyUIAttempt{attempt}}
	healthy := false
	adapter := &comfyUIAdapter{store: stateStore, upstream: upstream, transport: comfyUIRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if !healthy {
			return nil, io.ErrUnexpectedEOF
		}
		if strings.HasPrefix(r.URL.Path, "/history/") {
			return comfyUIDiagnosticResponse(`{}`), nil
		}
		queue := fmt.Sprintf(`{"queue_running":[[1,%q,{}, {"gpu_workload_supervisor":{"attemptId":%q}},[]]],"queue_pending":[]}`, attempt.PromptID, attempt.AttemptID)
		return comfyUIDiagnosticResponse(queue), nil
	})}
	diagnostics := comfyUIDiagnostics{}
	var reports []error
	observe := func(id, reason string) {
		diagnostics.observe(id, reason, func(err error) { reports = append(reports, err) })
	}
	if err := adapter.reconcileWithDiagnostics(context.Background(), observe); err == nil {
		t.Fatal("runtime failure accepted")
	}
	healthy = true
	if err := adapter.reconcileWithDiagnostics(context.Background(), observe); err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 {
		t.Fatal("empty page claimed recovery")
	}
	if err := adapter.reconcileWithDiagnostics(context.Background(), observe); err != nil {
		t.Fatal(err)
	}
	var recovery *ComfyUIRecovery
	if len(reports) != 2 || !errors.As(reports[1], &recovery) || len(stateStore.finished) != 0 {
		t.Fatalf("pending recovery: reports=%v completions=%v", reports, stateStore.finished)
	}
}
