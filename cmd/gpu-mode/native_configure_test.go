package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
)

func TestConfigureRequiresStoppedProxies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	proxy, err := lock.AcquireContext(context.Background(), path+".proxy.lock", true)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	gate, err := acquireProxyLifetimeLock(path, "configure", "")
	if err == nil {
		gate.Close()
		t.Fatal("catalog replacement permitted while legacy proxy can forward")
	}
	if err = proxy.Close(); err != nil {
		t.Fatal(err)
	}
	gate, err = acquireProxyLifetimeLock(path, "configure", "")
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	if another, err := lock.TryAcquire(path + ".proxy.lock"); err == nil {
		another.Close()
		t.Fatal("configure does not exclude proxy lifetime")
	}
}
