package setup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestLocalAIInstallationsAreDiscoveredWhileStopped(t *testing.T) {
	home := t.TempDir()
	paths := []string{"AI/ollama/0.40.2/bin/ollama", "AI/vllm-supervisor/.venv/bin/vllm"}
	for _, relative := range paths {
		path := filepath.Join(home, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("not executed"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	requests := relevantExecutablePaths(home)
	found := inspectInstalledExecutables(context.Background(), requests, os.Lstat, func(string, string) error { return nil }, filepath.EvalSymlinks)
	if len(found) != 2 {
		t.Fatalf("want two stopped installations, got %+v", found)
	}
	for _, candidate := range found {
		if !candidate.Recognized || !candidate.Installed || candidate.Binding == nil || candidate.Binding.Owned == nil {
			t.Fatalf("missing executable evidence: %+v", candidate)
		}
	}
}

func TestLocalAIDiscoveryBoundsVersionDirectoryAndRejectsFIFO(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "AI/ollama")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 33; i++ {
		if err := os.Mkdir(filepath.Join(root, fmt.Sprintf("0.%d", i)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(localAIExecutablePaths(home)); got != 8 {
		t.Fatalf("unbounded version scan: %d", got)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(root, 0600); err != nil {
		t.Fatal(err)
	}
	if got := len(localAIExecutablePaths(home)); got != 8 {
		t.Fatalf("FIFO became an installation root: %d", got)
	}
}
