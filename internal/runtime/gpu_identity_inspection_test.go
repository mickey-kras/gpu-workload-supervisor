package runtime

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestExternalInspectionRejectsUnqualifiedExplicitGPUSelections(t *testing.T) {
	script := comfyScriptFixture(t)
	for _, selection := range []string{
		"0", "12", "", "-1", "0,1", "GPU-01234567",
		testPhysicalUUID + ",GPU-ffffffff-ffff-ffff-ffff-ffffffffffff",
		"MIG-01234567-89ab-cdef-0123-456789abcdef", "MIG-" + testPhysicalUUID + "/1/0",
	} {
		t.Run(selection, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "external.service")
			raw := []byte("[Service]\nExecStart=/usr/bin/python3 " + script + "\nEnvironment=CUDA_VISIBLE_DEVICES=" + selection + "\n")
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			launch, err := inspectAutomaticLaunchSourcesWithValidator(path, "comfyui", nil, func(string) error { return nil })
			if !errors.Is(err, ErrLaunchUnsupported) || launch.Endpoint != "" || launch.Command != "" || launch.SHA256 != "" || launch.Environment != nil || launch.GPUUUID != "" {
				t.Fatalf("selection produced a usable binding: %+v, %v", launch, err)
			}
			wantUnresolved := selection == "0" || selection == "12"
			if errors.Is(err, ErrGPUIdentityUnresolved) != wantUnresolved {
				t.Fatalf("numeric discovery distinction lost: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, raw) {
				t.Fatalf("GPU inspection changed external source: %v", err)
			}
		})
	}
}

func TestExternalInspectionPreservesFullPhysicalGPUSelection(t *testing.T) {
	script := comfyScriptFixture(t)
	path := filepath.Join(t.TempDir(), "external.service")
	command := "/usr/bin/python3 " + script
	raw := []byte("[Service]\nExecStart=" + command + "\nEnvironment=CUDA_VISIBLE_DEVICES=" + testPhysicalUUID + "\n")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	launch, err := inspectAutomaticLaunchSourcesWithValidator(path, "comfyui", nil, func(string) error { return nil })
	if err != nil || launch.GPUUUID != testPhysicalUUID || launch.Environment["CUDA_VISIBLE_DEVICES"] != testPhysicalUUID || launch.Command != command {
		t.Fatalf("full physical identity lost: %+v, %v", launch, err)
	}
}
