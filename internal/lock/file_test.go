package lock

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestLockHelper(t *testing.T) {
	if os.Getenv("GPU_LOCK_HELPER") != "1" {
		return
	}
	path := os.Args[len(os.Args)-1]
	lock, err := Acquire(path)
	if err != nil {
		os.Exit(2)
	}
	defer lock.Close()
	if ready := os.Getenv("GPU_LOCK_READY"); ready != "" {
		if err := os.WriteFile(ready, nil, 0o600); err != nil {
			os.Exit(3)
		}
	}
	if os.Getenv("GPU_LOCK_HOLD") == "1" {
		time.Sleep(500 * time.Millisecond)
	}
}

func TestAcquireSerializesProcesses(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "state.lock")
	ready := filepath.Join(directory, "ready")
	first := exec.Command(os.Args[0], "-test.run=TestLockHelper", "--", path)
	first.Env = append(os.Environ(), "GPU_LOCK_HELPER=1", "GPU_LOCK_HOLD=1", "GPU_LOCK_READY="+ready)
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	defer first.Process.Kill()
	for deadline := time.Now().Add(time.Second); ; {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first process did not acquire lock")
		}
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	second := exec.CommandContext(ctx, os.Args[0], "-test.run=TestLockHelper", "--", path)
	second.Env = append(os.Environ(), "GPU_LOCK_HELPER=1")
	if err := second.Run(); ctx.Err() != context.DeadlineExceeded {
		t.Fatalf("second process was not blocked: err=%v context=%v", err, ctx.Err())
	}
	if err := first.Wait(); err != nil {
		t.Fatal(err)
	}
	third := exec.Command(os.Args[0], "-test.run=TestLockHelper", "--", path)
	third.Env = append(os.Environ(), "GPU_LOCK_HELPER=1")
	if output, err := third.CombinedOutput(); err != nil {
		t.Fatalf("acquire after release: %v: %s", err, output)
	}
}

func TestAcquireCreatesPrivateFilesAndRejectsSymlink(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "private")
	path := filepath.Join(directory, "state.lock")
	lock, err := Acquire(path)
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
	if _, err := Acquire(link); err == nil {
		t.Fatal("expected symlink rejection")
	}
}

func TestAcquireRejectsInvalidLockPath(t *testing.T) {
	if _, err := Acquire(""); err == nil {
		t.Fatal("empty lock path accepted")
	}
	parent := filepath.Join(t.TempDir(), "regular-file")
	if err := os.WriteFile(parent, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(filepath.Join(parent, "lock")); err == nil {
		t.Fatal("regular file accepted as lock parent")
	}
	var missing *File
	if err := missing.Close(); err != nil {
		t.Fatalf("nil lock close failed: %v", err)
	}
}
