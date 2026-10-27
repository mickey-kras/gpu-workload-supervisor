package setup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalAIInstallationsAreDiscoveredWhileStopped(t *testing.T) {
	home := t.TempDir()
	paths := []string{"AI/ollama/0.40.2/bin/ollama", "AI/ollama/bin/ollama", "AI/vllm-supervisor/.venv/bin/vllm"}
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
	if len(found) != 1 || found[0].App != "vllm" {
		t.Fatalf("want only the stopped vLLM installation, got %+v", found)
	}
	if !found[0].Recognized || !found[0].Installed || found[0].Binding == nil || found[0].Binding.Owned == nil {
		t.Fatalf("missing executable evidence: %+v", found[0])
	}
}

func TestLocalAIDiscoveryUsesFixedVLLMLocations(t *testing.T) {
	home := t.TempDir()
	paths := localAIExecutablePaths(home)
	if len(paths) != 4 {
		t.Fatalf("unexpected local AI locations: %+v", paths)
	}
	for _, path := range paths {
		if path.App != "vllm" || strings.Contains(path.Reference, "ollama") || !strings.HasPrefix(path.Reference, filepath.Join(home, "AI")+string(os.PathSeparator)) {
			t.Fatalf("unexpected local AI location: %+v", path)
		}
	}
}
