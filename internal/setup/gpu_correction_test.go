package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
)

func gpuCorrectionFixture(t *testing.T) (Backend, string, GPUCorrectionRequest, map[string]string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	unitRoot := filepath.Join(home, ".config/systemd/user")
	if err := os.MkdirAll(unitRoot, 0700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(home, "llama-server")
	model := filepath.Join(home, "model.gguf")
	for _, path := range []string{executable, model} {
		if err := os.WriteFile(path, []byte("fixture, never executed"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	command := executable + " -m " + model + " --host 127.0.0.1 --port 8080"
	unit := "llm.service"
	fragment := filepath.Join(unitRoot, unit)
	if err := os.WriteFile(fragment, []byte("[Service]\nType=exec\nRestart=no\nEnvironment=CUDA_VISIBLE_DEVICES=0\nExecStart="+command+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{"Id": unit, "LoadState": "loaded", "ActiveState": "inactive", "SubState": "dead", "NeedDaemonReload": "no", "FragmentPath": fragment, "ExecStart": "{ path=" + executable + " ; argv[]=" + command + " ; }", "Environment": "CUDA_VISIBLE_DEVICES=0", "ControlGroup": "/manager/app.slice/llm.service", "Slice": "app.slice"}
	b := Backend{gpuInventory: func(context.Context) ([]string, string) {
		return []string{"GPU-11111111-2222-3333-4444-555555555555"}, "available"
	}}
	b.runCommand = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != automaticSystemctlPath {
			t.Fatalf("unexpected executable %s", name)
		}
		if len(args) == 2 && args[1] == "daemon-reload" {
			return nil, errors.New("test reload failure")
		}
		if len(args) < 2 || args[1] != "show" {
			t.Fatalf("unexpected service mutation %v", args)
		}
		var out strings.Builder
		for key, value := range values {
			out.WriteString(key + "=" + value + "\n")
		}
		return []byte(out.String()), nil
	}
	return b, home, GPUCorrectionRequest{App: appLlamaCPP, Unit: unit}, values
}

func TestGPUCorrectionRequiresFreshConsentAndPreservesOriginalService(t *testing.T) {
	b, home, request, values := gpuCorrectionFixture(t)
	original, err := os.ReadFile(values["FragmentPath"])
	if err != nil {
		t.Fatal(err)
	}
	preview, err := b.CorrectGPU(context.Background(), home, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(preview.Path); !os.IsNotExist(err) {
		t.Fatal("preview wrote service override")
	}
	request.Confirm, request.Fingerprint = true, "stale"
	if _, err := b.CorrectGPU(context.Background(), home, request); err == nil {
		t.Fatal("stale consent accepted")
	}
	request.Fingerprint = preview.Fingerprint
	_, err = b.CorrectGPU(context.Background(), home, request)
	if err == nil || !strings.Contains(err.Error(), "correction saved; systemd reload failed") {
		t.Fatalf("reload failure hidden: %v", err)
	}
	content, err := os.ReadFile(preview.Path)
	if err != nil || string(content) != preview.Content {
		t.Fatalf("wrong correction: %s %v", content, err)
	}
	after, err := os.ReadFile(values["FragmentPath"])
	if err != nil || string(original) != string(after) {
		t.Fatal("original service changed")
	}
}

func TestGPUCorrectionRejectsActiveServiceAndAmbiguousInventory(t *testing.T) {
	b, home, request, values := gpuCorrectionFixture(t)
	values["ActiveState"] = "active"
	if _, err := b.CorrectGPU(context.Background(), home, request); err == nil {
		t.Fatal("active service accepted")
	}
	values["ActiveState"] = "inactive"
	b.gpuInventory = func(context.Context) ([]string, string) {
		return []string{"GPU-11111111-2222-3333-4444-555555555555", "GPU-22222222-2222-3333-4444-555555555555"}, "available"
	}
	if _, err := b.CorrectGPU(context.Background(), home, request); err == nil {
		t.Fatal("ambiguous GPU accepted")
	}
}

func TestGPUCorrectionPreviewDoesNotHideOtherLaunchFailures(t *testing.T) {
	b, home, request, values := gpuCorrectionFixture(t)
	if err := os.WriteFile(values["FragmentPath"], []byte("[Service]\nEnvironment=CUDA_VISIBLE_DEVICES=0\nExecStart=/missing/llama-server --model /missing/model\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.CorrectGPU(context.Background(), home, request); err == nil {
		t.Fatal("invalid executable accepted")
	}
}

func TestGPUCorrectionAppliesAndRechecksWithoutStartingService(t *testing.T) {
	b, home, request, values := gpuCorrectionFixture(t)
	originalRun := b.runCommand
	path := filepath.Join(home, ".config/systemd/user", request.Unit+".d", gpuCorrectionName)
	b.runCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if len(args) == 2 && args[1] == "daemon-reload" {
			values["DropInPaths"] = path
			values["Environment"] = "CUDA_VISIBLE_DEVICES=GPU-11111111-2222-3333-4444-555555555555"
			return nil, nil
		}
		if args[len(args)-1] == "-.slice" {
			return []byte("LoadState=loaded\nActiveState=active\nSubState=active\nControlGroup=/manager\n"), nil
		}
		return originalRun(ctx, name, args...)
	}
	plan, err := b.CorrectGPU(context.Background(), home, request)
	if err != nil {
		t.Fatal(err)
	}
	request.Confirm, request.Fingerprint = true, plan.Fingerprint
	result, err := b.CorrectGPU(context.Background(), home, request)
	if err != nil || !result.Applied {
		t.Fatalf("correction did not qualify: %+v %v", result, err)
	}
	if values["ActiveState"] != "inactive" {
		t.Fatal("service started")
	}
	if err := writeGPUCorrection(GPUCorrection{Path: path, Content: "replacement"}); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing override was replaced: %v", err)
	}
}

func TestGPUCorrectionRejectsCatalogManagedWithoutOperatorProfile(t *testing.T) {
	b, home, request, _ := gpuCorrectionFixture(t)
	ctx := context.Background()
	s := openStoreAt(t, deployment.DefaultStatePath())
	_, err := s.ReplaceCatalog(ctx, "", control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{
		ID: "llm", Label: "LLM", Adapter: "systemd", Unit: request.Unit,
		Cgroup: "/workloads/llm.service", HealthURL: "http://127.0.0.1:8080/health",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := os.Stat(filepath.Join(home, ".config/gpu-workload-supervisor/operator.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("operator profile unexpectedly present: %v", err)
	}
	if _, err := b.CorrectGPU(ctx, home, request); err == nil || !strings.Contains(err.Error(), "already managed") {
		t.Fatalf("catalog-managed service accepted without operator profile: %v", err)
	}
}
