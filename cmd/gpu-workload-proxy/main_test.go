package main

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
	workloadproxy "github.com/mickey-kras/gpu-workload-supervisor/internal/proxy"
)

func TestShutdownTimeoutKeepsLifetimeLockUntilHandlerExits(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "state.db.proxy.lock")
	shared, err := lock.AcquireShared(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	tracked := &activeHandler{handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release // Model an upstream call that has not yet returned after cancellation.
		w.WriteHeader(http.StatusNoContent)
	})}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: tracked}
	defer server.Close()
	go server.Serve(listener)
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		resp, err := http.Get("http://" + listener.Addr().String())
		if err == nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request never reached the handler")
	}
	shutdownDone := make(chan error, 1)
	go func() {
		err := shutdownAndDrain(server, tracked, 10*time.Millisecond)
		shutdownDone <- errors.Join(err, shared.Close())
	}()
	select {
	case err := <-shutdownDone:
		t.Fatalf("shutdown returned while handler was active: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if exclusive, err := lock.TryAcquire(lockPath); err == nil {
		exclusive.Close()
		t.Fatal("recovery could acquire lock while handler was active")
	} else if !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatalf("try acquire: %v", err)
	}
	close(release)
	select {
	case err := <-shutdownDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("shutdown error = %v, want deadline exceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not finish after handler exited")
	}
	select {
	case <-requestDone:
	case <-time.After(2 * time.Second):
		t.Fatal("request client did not finish")
	}
	exclusive, err := lock.TryAcquire(lockPath)
	if err != nil {
		t.Fatalf("recovery could not acquire lock after handler exited: %v", err)
	}
	exclusive.Close()
}

func TestProxyCapacityReservesCompletionAndRecovers(t *testing.T) {
	ordinaryStarted := make(chan struct{})
	completionStarted := make(chan struct{})
	releaseOrdinary := make(chan struct{})
	releaseCompletion := make(chan struct{})
	tracked := &activeHandler{maxOrdinary: 1, maxCompletion: 1, completionPath: "/finish",
		handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/finish" {
				close(completionStarted)
				<-releaseCompletion
			} else {
				close(ordinaryStarted)
				<-releaseOrdinary
			}
			w.WriteHeader(http.StatusNoContent)
		})}
	request := func(method, path string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		tracked.ServeHTTP(response, httptest.NewRequest(method, path, nil))
		return response
	}
	ordinaryDone := make(chan struct{})
	go func() { defer close(ordinaryDone); request(http.MethodPost, "/execute") }()
	<-ordinaryStarted
	busy := request(http.MethodGet, "/monitor")
	if busy.Code != http.StatusServiceUnavailable || busy.Header().Get("Retry-After") != "1" ||
		!strings.Contains(busy.Body.String(), "proxy_capacity_exceeded") {
		t.Fatalf("ordinary saturation response: %d %q", busy.Code, busy.Body.String())
	}
	completionDone := make(chan struct{})
	go func() { defer close(completionDone); request(http.MethodPost, "/finish") }()
	<-completionStarted
	if got := request(http.MethodPost, "/finish"); got.Code != http.StatusServiceUnavailable {
		t.Fatalf("completion saturation status = %d", got.Code)
	}
	if got := request(http.MethodGet, "/finish"); got.Code != http.StatusServiceUnavailable {
		t.Fatalf("non-completion method bypassed ordinary cap: %d", got.Code)
	}
	close(releaseOrdinary)
	<-ordinaryDone
	ordinaryStarted = make(chan struct{})
	// A released ordinary slot can accept a new request while completion is busy.
	ordinaryAgain := make(chan struct{})
	go func() { request(http.MethodPost, "/execute"); close(ordinaryAgain) }()
	<-ordinaryStarted
	<-ordinaryAgain
	close(releaseCompletion)
	<-completionDone
	completionStarted = make(chan struct{})
	if got := request(http.MethodPost, "/finish"); got.Code != http.StatusNoContent {
		t.Fatalf("completion slot was not released: %d", got.Code)
	}
}

func TestShutdownWaitsForHijackedHandler(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	tracked := &activeHandler{handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		close(started)
		<-release
	})}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: tracked}
	defer server.Close()
	go server.Serve(listener)
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: localhost\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request was not hijacked")
	}
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- shutdownAndDrain(server, tracked, 10*time.Millisecond) }()
	select {
	case err := <-shutdownDone:
		t.Fatalf("shutdown returned while hijacked handler was active: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-shutdownDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("shutdown error = %v, want deadline exceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not finish after upgraded handler exited")
	}
}

func TestShutdownClosesHijackedConnectionAndReleasesLifetimeLock(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "state.db.proxy.lock")
	shared, err := lock.AcquireShared(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer shared.Close()
	started := make(chan struct{})
	tracked := &activeHandler{handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n"))
		close(started)
		var buf [1]byte
		_, _ = conn.Read(buf[:]) // An upgraded stream blocked on its client.
	})}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: tracked}
	defer server.Close()
	go server.Serve(listener)
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Write([]byte("GET / HTTP/1.1\r\nHost: localhost\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upgrade did not start")
	}
	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- errors.Join(shutdownAndDrain(server, tracked, 20*time.Millisecond), shared.Close())
	}()
	select {
	case err := <-shutdownDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("shutdown error = %v, want deadline exceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("hijacked connection prevented shutdown")
	}
	exclusive, err := lock.TryAcquire(lockPath)
	if err != nil {
		t.Fatalf("recovery could not acquire lock after handler exited: %v", err)
	}
	exclusive.Close()
}

func TestShutdownClosesReverseProxyUpgrade(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "state.db.proxy.lock")
	shared, err := lock.AcquireShared(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer shared.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n"))
		var buf [1]byte
		_, _ = conn.Read(buf[:])
	}))
	defer upstream.Close()
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	tracked := &activeHandler{handler: httputil.NewSingleHostReverseProxy(target)}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: tracked}
	defer server.Close()
	go server.Serve(listener)
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write([]byte("GET /stream HTTP/1.1\r\nHost: localhost\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	status, err := bufio.NewReader(client).ReadString('\n')
	if err != nil || !strings.Contains(status, "101 Switching Protocols") {
		t.Fatalf("upgrade status = %q, error = %v", status, err)
	}
	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- errors.Join(shutdownAndDrain(server, tracked, 20*time.Millisecond), shared.Close())
	}()
	select {
	case err := <-shutdownDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("shutdown error = %v, want deadline exceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ReverseProxy upgraded stream prevented shutdown")
	}
	exclusive, err := lock.TryAcquire(lockPath)
	if err != nil {
		t.Fatalf("recovery could not acquire lock after upgraded stream ended: %v", err)
	}
	exclusive.Close()
}

func TestRoutesFlagRequiresCanonicalMethodAndPath(t *testing.T) {
	var routes routesFlag
	for _, value := range []string{"POST", ":/execute", "POST:execute", "POST:/a/../execute"} {
		if err := routes.Set(value); err == nil {
			t.Fatalf("accepted route %q", value)
		}
	}
	if err := routes.Set(" post :/execute"); err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0] != (workloadproxy.Route{Method: "POST", Path: "/execute"}) || routes.String() != "POST:/execute" {
		t.Fatalf("routes = %#v, string = %q", routes, routes.String())
	}
}

func TestProxyFlagValidation(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.db")
	tests := []struct {
		name, want string
		args       []string
	}{
		{"positional", "unexpected positional", []string{"extra"}},
		{"invalid retention", "completed-work-retention must be positive", []string{"-completed-work-retention", "0s"}},
		{"invalid ordinary capacity", "max-inflight and max-completion-inflight must be positive", []string{"-max-inflight", "0"}},
		{"invalid completion capacity", "max-inflight and max-completion-inflight must be positive", []string{"-max-completion-inflight", "0"}},
		{"public listener", "listen address must be loopback", []string{"-listen", "0.0.0.0:8090"}},
		{"malformed listener", "invalid listen address", []string{"-listen", "not-an-address"}},
		{"missing upstream", "upstream is required", nil},
		{"malformed upstream", "parse upstream", []string{"-upstream", "http://%"}},
		{"relative upstream", "upstream must be an absolute", []string{"-upstream", "/path"}},
		{"unknown workload", "workload must be text or media", []string{"-upstream", "http://127.0.0.1:1", "-workload", "other"}},
		{"no routes", "at least one execution route", []string{"-upstream", "http://127.0.0.1:1", "-workload", "media"}},
		{"invalid finish path", "completion path must be canonical", []string{"-upstream", "http://127.0.0.1:1", "-workload", "media", "-execute-route", "POST:/execute", "-completion-path", "/a/../finish"}},
		{"mutating read-only", "read-only routes require", []string{"-upstream", "http://127.0.0.1:1", "-workload", "media", "-execute-route", "POST:/execute", "-read-only-route", "POST:/monitor"}},
		{"repeated read-only collision", "routes overlap", []string{"-upstream", "http://127.0.0.1:1", "-workload", "media", "-execute-route", "GET:/execute", "-read-only-route", "GET:/monitor", "-read-only-route", "GET:/execute"}},
		{"malformed read-only", "route must use", []string{"-read-only-route", "GET:monitor"}},
		{"route collision", "routes overlap", []string{"-upstream", "http://127.0.0.1:1", "-workload", "media", "-execute-route", "POST:/execute", "-passthrough-route", "POST:/execute"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := os.Args
			os.Args = append([]string{"gpu-workload-proxy", "-state", statePath}, test.args...)
			t.Cleanup(func() { os.Args = before })
			if err := run(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("run() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestProxyCannotStartWhenListenerIsOccupied(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	before := os.Args
	os.Args = []string{"gpu-workload-proxy", "-state", filepath.Join(t.TempDir(), "state.db"),
		"-listen", listener.Addr().String(), "-upstream", "http://127.0.0.1:1",
		"-workload", "media", "-execute-route", "POST:/execute"}
	t.Cleanup(func() { os.Args = before })
	if err := run(); err == nil || !strings.Contains(err.Error(), "address already in use") {
		t.Fatalf("run with occupied listener = %v", err)
	}
}

func TestDefaultStatePathAndLoopback(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	if got := defaultStatePath(); got != filepath.Join(root, "gpu-workload-supervisor", "state.db") {
		t.Fatalf("state path = %q", got)
	}
	for _, address := range []string{"localhost:8090", "[::1]:8090", "127.0.0.1:8090"} {
		if err := validateLoopbackAddress(address); err != nil {
			t.Fatalf("%s: %v", address, err)
		}
	}
	for _, address := range []string{"example.com:8090", "[::]:8090"} {
		if err := validateLoopbackAddress(address); err == nil {
			t.Fatalf("accepted %s", address)
		}
	}
}
