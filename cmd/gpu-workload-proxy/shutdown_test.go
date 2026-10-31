package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	workloadproxy "github.com/mickey-kras/gpu-workload-supervisor/internal/proxy"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/vllmkey"
)

type shutdownNativeStore struct {
	workloadproxy.StateStore
	endpoint string
	keyFile  string
	started  chan struct{}
	release  chan struct{}
}

func (s *shutdownNativeStore) State(context.Context) (control.State, error) {
	return control.State{Owner: control.OwnerSupervisor}, nil
}
func (s *shutdownNativeStore) Catalog(context.Context) (control.CatalogSnapshot, error) {
	return control.CatalogSnapshot{Revision: "one", Catalog: control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{
		{ID: "media", NativeModel: &control.NativeModel{Runtime: "vllm", Model: "selected", Endpoint: s.endpoint, EnvironmentFile: s.keyFile}},
	}}}, nil
}
func (s *shutdownNativeStore) AdmitWorkTokenAtCatalog(context.Context, string, string, control.Workload, control.Fence, string) (string, error) {
	return "token", nil
}
func (s *shutdownNativeStore) FinishWorkToken(ctx context.Context, _ string, _ control.Workload, _ control.Fence, _ string, _ store.WorkOutcome) error {
	close(s.started)
	<-s.release
	return ctx.Err()
}

func waitShutdownBarrier(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown barrier timed out")
	}
}

func TestShutdownProxyDrainsHTTPBeforeNativePersistence(t *testing.T) {
	requested, terminal := make(chan struct{}), make(chan struct{})
	var releaseTerminal sync.Once
	defer releaseTerminal.Do(func() { close(terminal) })
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(requested)
		<-terminal
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	s := &shutdownNativeStore{endpoint: upstream.URL, keyFile: shutdownKeyFile(t), started: make(chan struct{}), release: make(chan struct{})}
	var releaseWrite sync.Once
	defer releaseWrite.Do(func() { close(s.release) })
	target, _ := url.Parse(upstream.URL)
	handler, err := workloadproxy.NewWithContext(context.Background(), s, workloadproxy.Config{
		Upstream: target, Workload: "media", ExecutionRoutes: []workloadproxy.Route{{Method: "POST", Path: "/v1/completions"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	tracked := workloadproxy.NewServer(handler, 1, 1, "")
	server := httptest.NewServer(tracked)
	defer func() {
		releaseTerminal.Do(func() { close(terminal) })
		releaseWrite.Do(func() { close(s.release) })
		server.Close()
	}()
	shuttingDown := make(chan struct{})
	server.Config.RegisterOnShutdown(func() { close(shuttingDown) })
	request, _ := http.NewRequest("POST", server.URL+"/v1/completions", strings.NewReader(`{"model":"selected"}`))
	request.Header.Set(workloadproxy.DefaultRequestIDHeader, "request-1")
	request.Header.Set(workloadproxy.DefaultFenceIDHeader, "11111111-1111-4111-8111-111111111111")
	request.Header.Set(workloadproxy.DefaultFenceEpochHeader, "1")
	forwarded := make(chan struct{})
	go func() {
		defer close(forwarded)
		response, err := server.Client().Do(request)
		if err != nil {
			t.Error(err)
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil || string(body) != "data: [DONE]\n\n" {
			t.Error("shutdown lost admitted terminal response")
		}
	}()
	waitShutdownBarrier(t, requested)
	drained := make(chan error, 1)
	go func() { drained <- shutdownProxy(tracked, server.Config, handler, 5*time.Second) }()
	waitShutdownBarrier(t, shuttingDown)
	releaseTerminal.Do(func() { close(terminal) })
	waitShutdownBarrier(t, s.started)
	waitShutdownBarrier(t, forwarded)
	select {
	case err := <-drained:
		t.Fatalf("shutdown released store ownership before persistence: %v", err)
	default:
	}
	releaseWrite.Do(func() { close(s.release) })
	select {
	case err := <-drained:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("native persistence did not drain")
	}
}

// shutdownKeyFile writes the private vLLM API key file that the proxy reads.
func shutdownKeyFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vllm.env")
	if err := os.WriteFile(path, []byte(vllmkey.Name+"="+strings.Repeat("s", 43)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
