package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
)

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
