package lock

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestLockHelper(t *testing.T) {
	if os.Getenv("GPU_LOCK_HELPER") != "1" {
		return
	}
	path := os.Args[len(os.Args)-1]
	if os.Getenv("GPU_LOCK_ROLE") == "contender" {
		unexpected, err := TryAcquire(path)
		if unexpected != nil {
			unexpected.Close()
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			t.Fatalf("expected contention with holder: %v", err)
		}
		// An exclusive holder must exclude shared acquisitions too. Checking only
		// TryAcquire would also accept a holder that accidentally took LOCK_SH.
		unexpected, err = acquire(path, unix.LOCK_SH|unix.LOCK_NB)
		if unexpected != nil {
			unexpected.Close()
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			t.Fatalf("expected shared acquisition to contend with exclusive holder: %v", err)
		}
		fmt.Fprintln(os.Stdout, "contended")
	}
	lock, err := acquire(path, unix.LOCK_EX)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	fmt.Fprintln(os.Stdout, "acquired")
	if os.Getenv("GPU_LOCK_ROLE") == "holder" {
		input := bufio.NewScanner(os.Stdin)
		if !input.Scan() || input.Text() != "release" {
			t.Fatalf("expected release command: %q (%v)", input.Text(), input.Err())
		}
		if err := lock.Close(); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintln(os.Stdout, "released")
	}
}

func TestAcquireSerializesProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.lock")
	// The deadline bounds hangs; only helper events establish acquisition and contention.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := func(role string) (*exec.Cmd, io.WriteCloser, *bufio.Scanner) {
		t.Helper()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLockHelper$", "--", path)
		cmd.Env = append(os.Environ(), "GPU_LOCK_HELPER=1", "GPU_LOCK_ROLE="+role)
		cmd.Stderr = os.Stderr
		input, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { input.Close() })
		output, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { output.Close() })
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if cmd.ProcessState == nil {
				cmd.Process.Kill()
				cmd.Wait()
			}
		})
		return cmd, input, bufio.NewScanner(output)
	}
	expect := func(output *bufio.Scanner, event string) {
		t.Helper()
		if !output.Scan() || output.Text() != event {
			t.Fatalf("expected helper event %q: got %q (scan=%v, context=%v)", event, output.Text(), output.Err(), ctx.Err())
		}
	}
	first, release, firstOutput := start("holder")
	expect(firstOutput, "acquired")
	second, _, secondOutput := start("contender")
	// This event requires both exclusive and shared probes to observe contention.
	expect(secondOutput, "contended")
	if _, err := fmt.Fprintln(release, "release"); err != nil {
		t.Fatal(err)
	}
	expect(firstOutput, "released")
	expect(secondOutput, "acquired")
	if err := first.Wait(); err != nil {
		t.Fatalf("holder: %v", err)
	}
	if err := second.Wait(); err != nil {
		t.Fatalf("contender acquire after release: %v", err)
	}
}

func TestSharedProxyLocksBlockExclusiveResolution(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.proxy.lock")
	first, err := AcquireContext(context.Background(), path, true)
	if err != nil {
		t.Fatal(err)
	}
	second, err := AcquireContext(context.Background(), path, true)
	if err != nil {
		t.Fatal(err)
	}
	if exclusive, err := TryAcquire(path); err == nil {
		exclusive.Close()
		t.Fatal("exclusive recovery lock succeeded with active proxies")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if exclusive, err := TryAcquire(path); err == nil {
		exclusive.Close()
		t.Fatal("exclusive recovery lock succeeded while second proxy is active")
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	exclusive, err := TryAcquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer exclusive.Close()
}

func TestAcquireCreatesPrivateFilesAndRejectsSymlink(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "private")
	path := filepath.Join(directory, "state.lock")
	lock, err := acquire(path, unix.LOCK_EX)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	directoryInfo, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if directoryInfo.Mode().Perm() != 0o700 || fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("modes directory=%#o file=%#o", directoryInfo.Mode().Perm(), fileInfo.Mode().Perm())
	}
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := acquire(link, unix.LOCK_EX); err == nil {
		t.Fatal("expected symlink rejection")
	}
}

func TestAcquireRejectsInvalidLockPath(t *testing.T) {
	if _, err := acquire("", unix.LOCK_EX); err == nil {
		t.Fatal("empty lock path accepted")
	}
	parent := filepath.Join(t.TempDir(), "regular-file")
	if err := os.WriteFile(parent, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := acquire(filepath.Join(parent, "lock"), unix.LOCK_EX); err == nil {
		t.Fatal("regular file accepted as lock parent")
	}
	var missing *File
	if err := missing.Close(); err != nil {
		t.Fatalf("nil lock close failed: %v", err)
	}
}

func TestContextLockTimesOutAndReleasesReadersIndependently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request.lock")
	first, err := AcquireContext(context.Background(), path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := AcquireContext(context.Background(), path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := AcquireContext(ctx, path, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second reader lost its lock: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	writer, err := AcquireContext(context.Background(), path, false)
	if err != nil {
		t.Fatal(err)
	}
	writer.Close()
	canceled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if _, err := AcquireContext(canceled, path, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled lock: %v", err)
	}
	if _, err := AcquireContext(context.Background(), "", false); err == nil {
		t.Fatal("empty lock accepted")
	}
}

func TestCloseIsIdempotentWhileAnotherLockIsHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.lock")
	first, err := acquire(path, unix.LOCK_EX)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := acquire(path, unix.LOCK_EX)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	done := make(chan error, 8)
	for i := 0; i < cap(done); i++ {
		go func() { done <- first.Close() }()
	}
	for i := 0; i < cap(done); i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if unexpected, err := TryAcquire(path); err == nil {
		unexpected.Close()
		t.Fatal("repeated close released another lock")
	}
}
