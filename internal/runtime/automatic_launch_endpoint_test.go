package runtime

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestComfyEqualsOptionsPreserveCommandAndEndpoint(t *testing.T) {
	script := comfyScriptFixture(t)
	directory := t.TempDir()
	config := filepath.Join(t.TempDir(), "models.yaml")
	if err := os.WriteFile(config, []byte("comfyui: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	command := "/usr/bin/python3 " + script + " --listen=::1 --port=9000 --output-directory=" + directory + " --temp-directory=" + directory + " --user-directory=" + directory + " --extra-model-paths-config=" + config + " --front-end-version latest --unknown-option=value"
	got, err := inspectAutomaticLaunch([]byte("[Service]\nExecStart="+command+"\n"), "comfyui", func(string) error { return nil })
	if err != nil || got.Endpoint != "http://[::1]:9000" || got.Command != command {
		t.Fatalf("equals options lost: %+v, %v", got, err)
	}
	if err := os.Chmod(directory, 0777); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectAutomaticLaunch([]byte("[Service]\nExecStart="+command+"\n"), "comfyui", func(string) error { return nil }); err == nil || !strings.Contains(err.Error(), directory) {
		t.Fatalf("equals path option bypassed qualification: %v", err)
	}
}

func TestComfyDeviceOptionsCannotOverrideInspectedGPUIdentity(t *testing.T) {
	script := comfyScriptFixture(t)
	for _, flags := range []string{"--cuda-device 1", "--cuda-device=1", "--cuda-d 1", "--cuda-d=1", "--default-device 1", "--default-device=1", "--default-d 1", "--default-d=1"} {
		t.Run(flags, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "external.service")
			raw := []byte("[Service]\nExecStart=/usr/bin/python3 " + script + " " + flags + "\nEnvironment=CUDA_VISIBLE_DEVICES=" + testPhysicalUUID + "\n")
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			for _, cleanup := range []bool{false, true} {
				launch, err := inspectAutomaticLaunchSourcesMode(path, "comfyui", nil, func(string) error { return nil }, cleanup)
				if !errors.Is(err, ErrLaunchUnsupported) || !strings.Contains(err.Error(), "overrides CUDA_VISIBLE_DEVICES") || launch.Endpoint != "" || launch.GPUUUID != "" || launch.Command != "" || launch.Environment != nil {
					t.Fatalf("device option escaped GPU qualification (cleanup=%v): %+v, %v", cleanup, launch, err)
				}
			}
			if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, raw) {
				t.Fatalf("inspection changed external source: %v", err)
			}
		})
	}
}

func TestComfyEndpointChangingOptionsCannotBeIgnored(t *testing.T) {
	script := comfyScriptFixture(t)
	for _, flags := range [][]string{
		{"--listen=0.0.0.0"},
		{"--port=0"},
		{"--port=bad"},
		{"--port=9000", "--port", "9001"},
		{"--listen", "127.0.0.1", "--listen=::1"},
		{"--tls-keyfile", "/key.pem", "--tls-certfile", "/cert.pem"},
		{"--tls-keyfile=/key.pem", "--tls-certfile=/cert.pem"},
		{"--tls-key=/key.pem"},
		{"--tls-cert", "/cert.pem"},
		{"--lis=0.0.0.0"},
		{"--lis", "0.0.0.0"},
		{"--por=9000"},
		{"--por", "9000"},
		{"--output-dir=/unqualified"},
		{"--temp-dir", "/unqualified"},
		{"--user-dir=/unqualified"},
		{"--extra-model-paths=/unqualified"},
		{"--", "--port=9000"},
	} {
		t.Run(strings.Join(flags, " "), func(t *testing.T) {
			args := append([]string{"/usr/bin/python3", script}, flags...)
			for _, cleanup := range []bool{false, true} {
				if endpoint, err := comfyLaunchEndpointMode(args, cleanup); !errors.Is(err, ErrLaunchUnsupported) {
					t.Fatalf("inferred endpoint %q for %v (cleanup=%v): %v", endpoint, flags, cleanup, err)
				}
			}
		})
	}
}
