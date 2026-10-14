package runtime

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExistingLaunchRecoveryRequiresFreshSafeSources(t *testing.T) {
	for _, app := range []string{"comfyui", "llama.cpp"} {
		t.Run(app, func(t *testing.T) {
			root := t.TempDir()
			cache := filepath.Join(root, "cache")
			if err := os.Mkdir(cache, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(cache, 0777); err != nil {
				t.Fatal(err)
			}
			model := filepath.Join(root, "existing.gguf")
			modelBytes := []byte("existing model is never read or changed by inspection")
			if err := os.WriteFile(model, modelBytes, 0600); err != nil {
				t.Fatal(err)
			}
			script := comfyScriptFixture(t)
			name, trusted := "python3", "/usr/bin/python3"
			arguments := " " + script + " --output-directory " + cache
			if app == "llama.cpp" {
				name, trusted = "llama-server", "/usr/bin/llama-server"
				arguments = " -m " + model + " --host 127.0.0.1 --port 8080"
			}
			unsafe := filepath.Join(root, name)
			if err := os.WriteFile(unsafe, []byte("never executed"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(unsafe, 0777); err != nil {
				t.Fatal(err)
			}
			unit := filepath.Join(root, "external.service")
			raw := []byte("[Service]\nRestart=on-failure\nEnvironment=CUDA_VISIBLE_DEVICES=0\nEnvironment=HF_HOME=" + cache + "\nEnvironment=PATH=" + root + "\nEnvironment=LD_LIBRARY_PATH=" + root + "\nExecStart=" + unsafe + arguments + "\n")
			if err := os.WriteFile(unit, raw, 0600); err != nil {
				t.Fatal(err)
			}
			found, err := InspectAutomaticLaunch(unit, app)
			if err == nil || found.Command != "" {
				t.Fatal("unsafe launch produced binding evidence", found, err)
			}
			for _, blocker := range []string{"Restart", "CUDA_VISIBLE_DEVICES", "HF_HOME", "PATH", "LD_LIBRARY_PATH", "group or world writable", "dependent loaded-state"} {
				if !strings.Contains(err.Error(), blocker) {
					t.Fatalf("missing independent blocker %s: %v", blocker, err)
				}
			}
			if got, err := os.ReadFile(unit); err != nil || !bytes.Equal(got, raw) {
				t.Fatal("inspection changed external source", err)
			}
			// The fixture owner explicitly repairs its data directory and selects
			// a different runtime. Inspection itself performs no repair.
			if err := os.Chmod(cache, 0700); err != nil {
				t.Fatal(err)
			}
			uuid := "GPU-01234567-89ab-cdef-0123-456789abcdef"
			corrected := []byte("[Service]\nRestart=no\nEnvironment=CUDA_VISIBLE_DEVICES=" + uuid + "\nEnvironment=HF_HOME=" + cache + "\nExecStart=" + trusted + arguments + "\n")
			if err := os.WriteFile(unit, corrected, 0600); err != nil {
				t.Fatal(err)
			}
			if app == "comfyui" {
				// Production interpreter, source and data-path qualification.
				found, err = InspectAutomaticLaunch(unit, app)
			} else {
				// CI has no llama-server installation. Substitute only its trust
				// verdict; this asserts parser/recovery wiring, not runtime trust.
				found, err = inspectAutomaticLaunchSourcesWithValidator(unit, app, nil, fixtureExecutableValidator)
			}
			if err != nil || found.Command != trusted+arguments || found.GPUUUID != uuid || found.Environment["HF_HOME"] != cache {
				t.Fatal("corrected source was not requalified", found, err)
			}
			for path, expected := range map[string][]byte{unit: corrected, model: modelBytes, unsafe: []byte("never executed")} {
				if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, expected) {
					t.Fatal("inspection changed application, model or source", err)
				}
			}
		})
	}
}
