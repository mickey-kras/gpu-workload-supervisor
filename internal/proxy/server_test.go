package proxy

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
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
	tracked := NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release // Model an upstream call that has not yet returned after cancellation.
		w.WriteHeader(http.StatusNoContent)
	}), 0, 0, "")
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
		err := tracked.ShutdownAndDrain(server, 10*time.Millisecond)
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
	tracked := NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/finish" {
			close(completionStarted)
			<-releaseCompletion
		} else {
			close(ordinaryStarted)
			<-releaseOrdinary
		}
		w.WriteHeader(http.StatusNoContent)
	}), 1, 1, "/finish")
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
	tracked := NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		close(started)
		<-release
	}), 0, 0, "")
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
	go func() { shutdownDone <- tracked.ShutdownAndDrain(server, 10*time.Millisecond) }()
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
	tracked := NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n"))
		close(started)
		var buf [1]byte
		_, _ = conn.Read(buf[:]) // An upgraded stream blocked on its client.
	}), 0, 0, "")
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
		shutdownDone <- errors.Join(tracked.ShutdownAndDrain(server, 20*time.Millisecond), shared.Close())
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

func TestStoppingServerRejectsNewRequests(t *testing.T) {
	tracked := NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), 0, 0, "")
	tracked.stop()
	response := httptest.NewRecorder()
	tracked.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/monitor", nil))
	if response.Code != http.StatusServiceUnavailable ||
		!strings.Contains(response.Body.String(), "proxy is shutting down") {
		t.Fatalf("request during shutdown: %d %q", response.Code, response.Body.String())
	}
}

func TestShutdownIdleServerDrainsImmediately(t *testing.T) {
	tracked := NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), 0, 0, "")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: tracked}
	defer server.Close()
	go server.Serve(listener)
	start := time.Now()
	if err := tracked.ShutdownAndDrain(server, 5*time.Second); err != nil {
		t.Fatalf("idle shutdown: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("idle shutdown took %v", elapsed)
	}
}

func TestResponseControllerDeadlineReachesUnderlyingConnection(t *testing.T) {
	tracked := NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(time.Minute)); err != nil {
			http.Error(w, "deadline unsupported", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}), 0, 0, "")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: tracked}
	defer server.Close()
	go server.Serve(listener)
	resp, err := http.Get("http://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("ResponseController through tracking writer: %d", resp.StatusCode)
	}
}

func TestHijackOnNonHijackableWriterFallsBack(t *testing.T) {
	tracked := NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, _, err := w.(http.Hijacker).Hijack(); err != nil {
			http.Error(w, "upgrade unsupported", http.StatusNotImplemented)
			return
		}
	}), 0, 0, "")
	response := httptest.NewRecorder()
	tracked.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/stream", nil))
	if response.Code != http.StatusNotImplemented {
		t.Fatalf("non-hijackable writer status = %d", response.Code)
	}
}

func TestShutdownClosesConnectionHijackedAfterDrainDeadline(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	connResult := make(chan error, 1)
	tracked := NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			connResult <- err
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		var buf [1]byte
		_, err = conn.Read(buf[:])
		connResult <- err
	}), 0, 0, "")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: tracked}
	defer server.Close()
	go server.Serve(listener)
	go func() {
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
	go func() { shutdownDone <- tracked.ShutdownAndDrain(server, 20*time.Millisecond) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		tracked.mu.Lock()
		closing := tracked.closingHijacked
		tracked.mu.Unlock()
		if closing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("shutdown never began closing hijacked connections")
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	select {
	case err := <-connResult:
		if err == nil {
			t.Fatal("connection hijacked after shutdown began was still usable")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("hijacked handler did not finish")
	}
	select {
	case err := <-shutdownDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("shutdown error = %v, want deadline exceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not finish after hijacked handler exited")
	}
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
	tracked := NewServer(httputil.NewSingleHostReverseProxy(target), 0, 0, "")
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
		shutdownDone <- errors.Join(tracked.ShutdownAndDrain(server, 20*time.Millisecond), shared.Close())
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
