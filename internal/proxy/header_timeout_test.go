package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

func headerTimeoutServer(t *testing.T, f *contractFixture, upstream string, timeout time.Duration) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	target, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewWithContext(context.Background(), f.store, Config{
		Upstream: target, Workload: control.WorkloadMedia, ResponseHeaderTimeout: timeout,
		ExecutionRoutes: []Route{{Method: http.MethodPost, Path: "/execute"}},
		ReadOnlyRoutes:  []Route{{Method: http.MethodGet, Path: "/monitor"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	tracked := NewServer(handler, 1, 1, DefaultCompletionPath)
	done := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tracked.ServeHTTP(w, r)
		if r.URL.Path == "/execute" {
			done <- struct{}{}
		}
	}))
	server.Client().Timeout = 5 * time.Second
	t.Cleanup(server.Close)
	return server, done
}

func waitHeaderRequest(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("header-wait request did not leave its handler")
	}
}

func assertMonitorStatus(t *testing.T, server *httptest.Server, want int) {
	t.Helper()
	response, err := server.Client().Get(server.URL + "/monitor")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != want {
		t.Fatalf("monitor status = %d, want %d", response.StatusCode, want)
	}
}

func TestResponseHeaderTimeoutReleasesCapacityWithoutFinishingWork(t *testing.T) {
	f := newContractFixture(t)
	previousToken, err := f.store.AdmitWorkToken(context.Background(), "previous", "", control.WorkloadMedia, f.state.LeaseFence)
	if err != nil {
		t.Fatal(err)
	}
	correlation := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/monitor" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		correlation <- r.Header.Get(DefaultRegistrationTokenHeader)
		<-r.Context().Done()
	}))
	t.Cleanup(upstream.Close)
	server, done := headerTimeoutServer(t, f, upstream.URL, time.Second)
	responseResult := make(chan *http.Response, 1)
	errorResult := make(chan error, 1)
	request := f.request(context.Background(), server.URL, "stalled")
	go func() {
		response, err := server.Client().Do(request)
		responseResult <- response
		errorResult <- err
	}()
	var token string
	select {
	case token = <-correlation:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not reach upstream")
	}
	assertMonitorStatus(t, server, http.StatusServiceUnavailable)
	// The previous request's verified callback still fits while ordinary HTTP
	// capacity is held by the upstream that never returns headers.
	f.finish(server.URL, finishRequest{"previous", previousToken, f.state.LeaseFence, store.WorkCompleted}, http.StatusNoContent)
	f.assertWork("previous", previousToken, true)
	f.assertWork("stalled", token, false)

	waitHeaderRequest(t, done)
	response := <-responseResult
	if err := <-errorResult; err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("stalled header status = %d, want 502", response.StatusCode)
	}
	assertMonitorStatus(t, server, http.StatusNoContent)
	f.assertWork("stalled", token, false)
	// A timeout cannot retire uncertain backend work, including across restart.
	server.Close()
	f.reopen()
	f.assertWork("stalled", token, false)
}

func TestClientCancellationDuringHeaderWaitReleasesCapacity(t *testing.T) {
	f := newContractFixture(t)
	correlation := make(chan string, 1)
	upstreamCanceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/monitor" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		correlation <- r.Header.Get(DefaultRegistrationTokenHeader)
		<-r.Context().Done()
		close(upstreamCanceled)
	}))
	t.Cleanup(upstream.Close)
	server, done := headerTimeoutServer(t, f, upstream.URL, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := f.request(ctx, server.URL, "canceled")
	errorsSeen := make(chan error, 1)
	go func() {
		response, err := server.Client().Do(request)
		if response != nil {
			response.Body.Close()
		}
		errorsSeen <- err
	}()
	var token string
	select {
	case token = <-correlation:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not reach upstream")
	}
	cancel()
	waitHeaderRequest(t, done)
	waitHeaderRequest(t, upstreamCanceled)
	if err := <-errorsSeen; !errors.Is(err, context.Canceled) {
		t.Fatalf("client cancellation = %v", err)
	}
	assertMonitorStatus(t, server, http.StatusNoContent)
	f.assertWork("canceled", token, false)
}

func TestResponseHeaderTimeoutAllowsSlowResponsesAndStreaming(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		name := "slow non-streaming response"
		if streaming {
			name = "body outlives header timeout"
		}
		t.Run(name, func(t *testing.T) {
			f := newContractFixture(t)
			const headerTimeout = 200 * time.Millisecond
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				delay := headerTimeout / 4
				if streaming {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: started\n\n")
					w.(http.Flusher).Flush()
					delay = 2 * headerTimeout
				}
				select {
				case <-time.After(delay):
					_, _ = io.WriteString(w, "complete")
				case <-r.Context().Done():
				}
			}))
			t.Cleanup(upstream.Close)
			server, done := headerTimeoutServer(t, f, upstream.URL, headerTimeout)
			response, err := server.Client().Do(f.request(context.Background(), server.URL, "slow"))
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if streaming {
				first := make([]byte, len("data: started\n\n"))
				if _, err := io.ReadFull(response.Body, first); err != nil || string(first) != "data: started\n\n" {
					t.Fatalf("first stream event = %q, error = %v", first, err)
				}
			}
			body, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != http.StatusOK || string(body) != "complete" {
				t.Fatalf("response = %d %q, error = %v", response.StatusCode, body, err)
			}
			waitHeaderRequest(t, done)
			f.assertWork("slow", response.Header.Get(DefaultRegistrationTokenHeader), false)
		})
	}
}

func TestNativeHeaderTimeoutDoesNotSupplyTerminalEvidence(t *testing.T) {
	f := newTerminalFixtureWith(t, "vllm", "/v1/completions", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	transport := f.handler.proxy.Transport.(upstreamKeyTransport).base.(*http.Transport)
	transport.ResponseHeaderTimeout = 50 * time.Millisecond
	t.Cleanup(transport.CloseIdleConnections)
	response := f.execute(t, "/v1/completions", `{"model":"selected"}`)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("native timeout status = %d, want 502", response.Code)
	}
	if len(f.store.admitted) != 1 || len(f.store.finished) != 0 {
		t.Fatalf("native timeout changed work: admitted=%v, finished=%v", f.store.admitted, f.store.finished)
	}
}
