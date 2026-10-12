package setup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

func TestNumericCUDAVisibilityPrepareAndDiscoveryPreserveExternalService(t *testing.T) {
	b, draft, metadata := automaticFixture(t, appLlamaCPP)
	path := filepath.Join(t.TempDir(), "external.service")
	raw := []byte("[Service]\nType=exec\nRestart=no\nEnvironment=CUDA_VISIBLE_DEVICES=0\nExecStartPre=/usr/bin/true\nExecStart=/usr/bin/llama-server -m /models/a.gguf --host 127.0.0.1 --port 8080 --parallel 2\n")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	metadata[draft.Binding.Unit]["FragmentPath"] = path
	metadata[draft.Binding.Unit]["Environment"] = "CUDA_VISIBLE_DEVICES=0"
	// Use production source inspection: no injected GPU-resolution error or
	// assumption that a management inventory index is a CUDA ordinal.
	b.inspectAutomatic = nil
	_, err := b.Prepare(context.Background(), PrepareRequest{Draft: draft})
	if !errors.Is(err, gpuruntime.ErrGPUIdentityUnresolved) || strings.Count(err.Error(), gpuruntime.ErrGPUIdentityUnresolved.Error()) != 1 {
		t.Fatalf("prepare lost or duplicated unresolved identity explanation: %v", err)
	}
	result := Discovery{}
	b.discoverUnit(context.Background(), &result, draft.Binding.Unit)
	if len(result.Applications) != 1 {
		t.Fatalf("discovered installation disappeared: %+v", result)
	}
	found := result.Applications[0]
	if found.ConfigurationStatus != "gpu-unresolved" || found.InstanceStatus != "not-running" || found.Binding != nil || found.Recognized {
		t.Fatalf("unresolved mapping claimed failed installation or permitted adoption: %+v", found)
	}
	if !strings.Contains(found.NextStep, "full UUID") || strings.Contains(found.NextStep, "unsupported existing launch") {
		t.Fatalf("discovery lost actionable GPU guidance: %q", found.NextStep)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatal("prepare/discovery changed the external unit", err)
	}
}
