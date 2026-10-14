package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
)

// Run the real flag parsing and serving path in a separate process so signals
// cannot cancel another test's service context.
func TestProxyServiceHelper(t *testing.T) {
	if os.Getenv("GPU_PROXY_SERVICE_HELPER") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"gpu-workload-proxy"}, os.Args[i+1:]...)
			break
		}
	}
	if err := os.WriteFile(os.Getenv("GPU_PROXY_SERVICE_READY"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type proxyServiceProcess struct {
	cmd    *exec.Cmd
	done   chan struct{}
	output bytes.Buffer
	err    error
}

func waitProxyCondition(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("proxy condition did not become true")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func startProxyService(t *testing.T, statePath, address, upstream string) *proxyServiceProcess {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "ready")
	p := &proxyServiceProcess{done: make(chan struct{})}
	p.cmd = exec.Command(os.Args[0], "-test.run=^TestProxyServiceHelper$", "--",
		"-state", statePath, "-listen", address, "-upstream", upstream,
		"-workload", "media", "-execute-route", "POST:/execute",
		"-read-only-route", "GET:/monitor", "-shutdown-timeout", "5s")
	p.cmd.Env = append(os.Environ(), "GPU_PROXY_SERVICE_HELPER=1", "GPU_PROXY_SERVICE_READY="+ready)
	p.cmd.Stdout, p.cmd.Stderr = &p.output, &p.output
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		p.err = p.cmd.Wait()
		close(p.done)
	}()
	t.Cleanup(func() {
		_ = p.cmd.Process.Kill()
		<-p.done
	})
	waitProxyCondition(t, 5*time.Second, func() bool {
		_, err := os.Stat(ready)
		return err == nil
	})
	return p
}

func (p *proxyServiceProcess) wait(t *testing.T, timeout time.Duration) (error, string) {
	t.Helper()
	select {
	case <-p.done:
		return p.err, p.output.String()
	case <-time.After(timeout):
		t.Fatal("proxy process did not exit")
		return nil, ""
	}
}

func unusedProxyAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func TestProxyServiceStartupWaitsAndStopsForMaintenance(t *testing.T) {
	for _, reason := range []string{"cancellation", "timeout"} {
		t.Run(reason, func(t *testing.T) {
			statePath := filepath.Join(t.TempDir(), "state.db")
			holder, err := lock.TryAcquire(statePath + ".proxy.lock")
			if err != nil {
				t.Fatal(err)
			}
			defer holder.Close()
			address := unusedProxyAddress(t)
			p := startProxyService(t, statePath, address, "http://127.0.0.1:1")
			select {
			case <-p.done:
				t.Fatal("proxy did not wait for maintenance")
			case <-time.After(200 * time.Millisecond):
			}
			if _, err := os.Stat(statePath); !os.IsNotExist(err) {
				t.Fatalf("production startup touched state before lock acquisition: %v", err)
			}
			want := "context deadline exceeded"
			if reason == "cancellation" {
				want = "context canceled"
				if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
			}
			wait := 5 * time.Second
			if reason == "timeout" {
				wait = proxyStartupLockTimeout + 5*time.Second
			}
			if err, output := p.wait(t, wait); err == nil || !strings.Contains(output, want) || !strings.Contains(output, "exclusive maintenance") {
				t.Fatalf("production startup exit = %v, output = %q", err, output)
			}
			if _, err := os.Stat(statePath); !os.IsNotExist(err) {
				t.Fatalf("failed startup initialized state: %v", err)
			}
			if err := holder.Close(); err != nil {
				t.Fatal(err)
			}
			maintenance, err := lock.TryAcquire(statePath + ".proxy.lock")
			if err != nil {
				t.Fatalf("failed production startup leaked lock: %v", err)
			}
			maintenance.Close()
		})
	}
}

func TestProxyServiceRetainsLockThroughSignalAndHandlerDrain(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.db")
	holder, err := lock.TryAcquire(statePath + ".proxy.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "started\n")
		w.(http.Flusher).Flush()
		close(entered)
		<-release
		_, _ = io.WriteString(w, "finished\n")
	}))
	defer func() {
		releaseOnce.Do(func() { close(release) })
		upstream.Close()
	}()
	address := unusedProxyAddress(t)
	p := startProxyService(t, statePath, address, upstream.URL)
	select {
	case <-p.done:
		t.Fatal("proxy did not wait for exclusive maintenance")
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("production startup initialized state while maintenance held lock: %v", err)
	}
	if err := holder.Close(); err != nil {
		t.Fatal(err)
	}
	waitProxyCondition(t, 5*time.Second, func() bool {
		connection, err := net.DialTimeout("tcp", address, 20*time.Millisecond)
		if err == nil {
			connection.Close()
		}
		return err == nil
	})
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Get("http://" + address + "/monitor")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not reach upstream")
	}
	if unexpected, err := lock.TryAcquire(statePath + ".proxy.lock"); err == nil {
		unexpected.Close()
		t.Fatal("serving proxy did not retain lifetime lock")
	}
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	// A closed listener proves the real signal handler entered shutdown. Keep
	// the response blocked while observing ownership after asynchronous work
	// has had time to run, rather than checking only once immediately at cancel.
	waitProxyCondition(t, time.Second, func() bool {
		connection, err := net.DialTimeout("tcp", address, 20*time.Millisecond)
		if err == nil {
			connection.Close()
		}
		return err != nil
	})
	for deadline := time.Now().Add(300 * time.Millisecond); time.Now().Before(deadline); {
		if unexpected, err := lock.TryAcquire(statePath + ".proxy.lock"); err == nil {
			unexpected.Close()
			t.Fatal("signal released lifetime lock before active handler drained")
		} else if !errors.Is(err, syscall.EWOULDBLOCK) {
			t.Fatal(err)
		}
		select {
		case <-p.done:
			t.Fatal("proxy exited before active handler drained")
		case <-time.After(10 * time.Millisecond):
		}
	}
	releaseOnce.Do(func() { close(release) })
	if body, err := io.ReadAll(response.Body); err != nil || string(body) != "started\nfinished\n" {
		t.Fatalf("admitted response lost during drain: %q, %v", body, err)
	}
	if err, output := p.wait(t, 5*time.Second); err != nil {
		t.Fatalf("drained proxy exit = %v, output = %q", err, output)
	}
	maintenance, err := lock.TryAcquire(statePath + ".proxy.lock")
	if err != nil {
		t.Fatalf("drained proxy retained lifetime lock: %v", err)
	}
	maintenance.Close()
}

func TestProxyStartupLockStopsWhileMaintenanceIsActive(t *testing.T) {
	for _, reason := range []string{"timeout", "cancellation"} {
		t.Run(reason, func(t *testing.T) {
			statePath := filepath.Join(t.TempDir(), "state.db")
			holder, err := lock.TryAcquire(statePath + ".proxy.lock")
			if err != nil {
				t.Fatal(err)
			}
			defer holder.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			timeout := 40 * time.Millisecond
			want := context.DeadlineExceeded
			if reason == "cancellation" {
				timeout = time.Second
				want = context.Canceled
			}
			done := make(chan error, 1)
			go func() {
				shared, err := acquireProxyLifetimeLock(ctx, statePath, timeout)
				if shared != nil {
					shared.Close()
				}
				done <- err
			}()
			if reason == "cancellation" {
				select {
				case err := <-done:
					t.Fatalf("startup did not wait for maintenance: %v", err)
				case <-time.After(20 * time.Millisecond):
				}
				cancel()
			}
			select {
			case err := <-done:
				if !errors.Is(err, want) || !strings.Contains(err.Error(), "exclusive maintenance") {
					t.Fatalf("startup error = %v, want clear contention error wrapping %v", err, want)
				}
			case <-time.After(time.Second):
				t.Fatal("startup did not stop waiting")
			}
			if _, err := os.Stat(statePath); !os.IsNotExist(err) {
				t.Fatalf("startup touched state before acquiring its lock: %v", err)
			}
			if err := holder.Close(); err != nil {
				t.Fatal(err)
			}
			maintenance, err := lock.TryAcquire(statePath + ".proxy.lock")
			if err != nil {
				t.Fatalf("canceled startup retained a shared lock: %v", err)
			}
			maintenance.Close()
		})
	}
}

func TestProxyStartupLockAcquiresAfterMaintenanceAndRetainsLifetime(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.db")
	holder, err := lock.TryAcquire(statePath + ".proxy.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		shared *lock.File
		err    error
	}
	done := make(chan result, 1)
	go func() {
		shared, err := acquireProxyLifetimeLock(ctx, statePath, time.Second)
		done <- result{shared, err}
	}()
	select {
	case got := <-done:
		got.shared.Close()
		t.Fatalf("startup did not wait for exclusive maintenance: %v", got.err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := holder.Close(); err != nil {
		t.Fatal(err)
	}
	var shared *lock.File
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		shared = got.shared
	case <-time.After(time.Second):
		t.Fatal("startup did not acquire released lock")
	}
	defer shared.Close()
	// The helper has canceled its startup deadline; even cancellation of the
	// service context must leave ownership with the caller until handlers drain.
	cancel()
	if unexpected, err := lock.TryAcquire(statePath + ".proxy.lock"); err == nil {
		unexpected.Close()
		t.Fatal("startup context cancellation released the lifetime lock")
	}
	if err := shared.Close(); err != nil {
		t.Fatal(err)
	}
	maintenance, err := lock.TryAcquire(statePath + ".proxy.lock")
	if err != nil {
		t.Fatalf("lifetime lock did not release on close: %v", err)
	}
	maintenance.Close()
}
