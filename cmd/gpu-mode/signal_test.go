package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
	"golang.org/x/sys/unix"
)

func TestPruneAuditSignalHelper(t *testing.T) {
	if os.Getenv("GPU_PRUNE_SIGNAL_HELPER") != "1" {
		return
	}
	os.Args = []string{"gpu-mode", "-state", os.Getenv("GPU_PRUNE_SIGNAL_STATE"), "-audit-before", "2020-01-01T00:00:00Z", "prune-audit"}
	main()
}

func TestPruneAuditInterruptedWhileControllerLockHeld(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.db")
	held, err := lock.Acquire(statePath + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	notify, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(notify)
	if _, err := unix.InotifyAddWatch(notify, statePath+".lock", unix.IN_OPEN); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPruneAuditSignalHelper$")
	child.Env = append(os.Environ(), "GPU_PRUNE_SIGNAL_HELPER=1", "GPU_PRUNE_SIGNAL_STATE="+statePath)
	var output bytes.Buffer
	child.Stdout, child.Stderr = &output, &output
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			child.Process.Kill()
			child.Wait()
		}
	}()

	// The child opens this lock only after installing its signal context. Watching
	// IN_OPEN proves it reached acquisition while the parent's lock stays held.
	events := make([]byte, 4096)
	for {
		n, err := unix.Read(notify, events)
		if n > 0 {
			break
		}
		if err != nil && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatal("child never reached controller lock")
		case <-time.After(time.Millisecond):
		}
	}
	if err := child.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	err = child.Wait()
	waited = true
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || ctx.Err() != nil || !strings.Contains(output.String(), "context canceled") {
		t.Fatalf("interrupt did not cancel lock wait: err=%v context=%v output=%s", err, ctx.Err(), output.String())
	}
	if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("interrupted command touched database: %v", err)
	}
}
