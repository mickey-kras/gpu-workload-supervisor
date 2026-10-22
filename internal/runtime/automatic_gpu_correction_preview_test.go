package runtime

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

const previewCorrectionUUID = "GPU-11111111-2222-3333-4444-555555555555"

func writeCorrectionPreviewFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0700); err != nil {
		t.Fatal(err)
	}
}

func TestPreviewAutomaticGPUCorrectionPreservesEffectiveLaunchAndSources(t *testing.T) {
	for _, app := range []string{"llama.cpp", "comfyui"} {
		t.Run(app, func(t *testing.T) {
			home := t.TempDir()
			executable := filepath.Join(home, "llama-server")
			model := filepath.Join(home, "model.gguf")
			writeCorrectionPreviewFile(t, model, "model fixture")
			command := executable + " -m " + model + " --host 127.0.0.1 --port 8080"
			endpoint := "http://127.0.0.1:8080"
			if app == "comfyui" {
				executable = filepath.Join(home, "python3")
				script := filepath.Join(home, "ComfyUI", "main.py")
				writeCorrectionPreviewFile(t, script, "raise RuntimeError('must not run')")
				command = executable + " " + script + " --listen 127.0.0.1 --port 8188"
				endpoint = "http://127.0.0.1:8188"
			}
			writeCorrectionPreviewFile(t, executable, "fixture, never executed")
			fragment := filepath.Join(home, "app.service")
			original := "[Service]\nType=exec\nRestart=no\nEnvironment=CUDA_VISIBLE_DEVICES=0\nExecStart=" + command + "\n"
			writeCorrectionPreviewFile(t, fragment, original)
			dropIn := filepath.Join(home, "app.service.d", "10-existing.conf")
			dropInContent := "[Service]\nEnvironment=CUDA_VISIBLE_DEVICES=1\nWorkingDirectory=" + home + "\n"
			writeCorrectionPreviewFile(t, dropIn, dropInContent)
			result, err := PreviewAutomaticGPUCorrection(fragment, app, []string{dropIn}, previewCorrectionUUID)
			if err != nil {
				t.Fatal(err)
			}
			if result.GPUUUID != previewCorrectionUUID || result.Environment["CUDA_VISIBLE_DEVICES"] != previewCorrectionUUID {
				t.Fatalf("proposed final GPU override not effective: %+v", result)
			}
			if result.Command != command || result.Endpoint != endpoint || result.WorkingDirectory != home {
				t.Fatalf("effective launch changed: %+v", result)
			}
			if app == "llama.cpp" && result.Model != model {
				t.Fatalf("model = %q", result.Model)
			}
			if result.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(original))) {
				t.Fatal("fragment hash includes synthetic override")
			}
			if len(result.DropIns) != 1 || result.DropIns[0].Path != dropIn || result.DropIns[0].SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(dropInContent))) {
				t.Fatalf("original drop-in evidence not retained: %+v", result.DropIns)
			}
			for path, want := range map[string]string{fragment: original, dropIn: dropInContent} {
				got, err := os.ReadFile(path)
				if err != nil || string(got) != want {
					t.Fatalf("source changed: %s: %q, %v", path, got, err)
				}
			}
			entries, err := os.ReadDir(filepath.Dir(dropIn))
			if err != nil || len(entries) != 1 {
				t.Fatalf("preview wrote a drop-in: %v, %v", entries, err)
			}
		})
	}
}

func TestPreviewAutomaticGPUCorrectionRejectsInvalidLaunch(t *testing.T) {
	home := t.TempDir()
	executable := filepath.Join(home, "llama-server")
	writeCorrectionPreviewFile(t, executable, "fixture, never executed")
	fragment := filepath.Join(home, "app.service")
	if _, err := PreviewAutomaticGPUCorrection(fragment, "llama.cpp", nil, "0"); !errors.Is(err, ErrGPUIdentityUnresolved) {
		t.Fatalf("invalid UUID accepted or source read first: %v", err)
	}
	if _, err := PreviewAutomaticGPUCorrection(fragment, "llama.cpp", nil, previewCorrectionUUID); err == nil {
		t.Fatal("missing source accepted")
	}
	for _, tc := range []struct {
		name, content string
		want          error
	}{
		{"unsupported directive", "[Service]\nEnvironmentFile=/missing/environment\nExecStart=" + executable + " -m /missing/model.gguf\n", ErrLaunchUnsupported},
		{"missing model", "[Service]\nEnvironment=CUDA_VISIBLE_DEVICES=0\nExecStart=" + executable + " -m " + filepath.Join(home, "missing.gguf") + " --host 127.0.0.1 --port 8080\n", ErrModelUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeCorrectionPreviewFile(t, fragment, tc.content)
			if _, err := PreviewAutomaticGPUCorrection(fragment, "llama.cpp", nil, previewCorrectionUUID); !errors.Is(err, tc.want) {
				t.Fatalf("GPU correction hid launch failure: %v", err)
			}
		})
	}
}
