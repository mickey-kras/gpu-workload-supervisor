package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAutomaticLaunchEvidence(t *testing.T) {
	script := comfyScriptFixture(t)
	dir := t.TempDir()
	model := filepath.Join(dir, "a.gguf")
	if err := os.WriteFile(model, []byte("model"), 0600); err != nil {
		t.Fatal(err)
	}
	tests := []struct{ app, command, env, endpoint, model string }{
		{"comfyui", "/usr/bin/python3 " + script, "", "http://127.0.0.1:8188", ""},
		{"comfyui", "/usr/bin/python3 " + script + " --listen ::1 --port 9000 --disable-auto-launch", "", "http://[::1]:9000", ""},
		{"ollama", "/usr/bin/ollama serve", "Environment=OLLAMA_NO_CLOUD=1\nEnvironment=OLLAMA_HOST=127.0.0.1:11434\n", "http://127.0.0.1:11434", ""},
		{"llama.cpp", "/usr/bin/llama-server -m " + model + " --host 127.0.0.1 --port 8080 --alias chat --ctx-size 4096", "", "http://127.0.0.1:8080", "chat"},
		{"vllm", "/usr/bin/vllm serve " + dir + " --host 127.0.0.1 --port 8000", "", "http://127.0.0.1:8000", dir},
	}
	for _, tt := range tests {
		t.Run(tt.app+tt.endpoint, func(t *testing.T) {
			raw := []byte("[Service]\nType=exec\nRestart=no\n" + tt.env + "ExecStart=" + tt.command + "\n")
			got, err := inspectAutomaticLaunch(raw, tt.app, func(string) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			if got.Endpoint != tt.endpoint || got.Model != tt.model || got.Command != tt.command || len(got.SHA256) != 64 {
				t.Fatalf("%+v", got)
			}
		})
	}
}
func TestAutomaticLaunchRefusesAmbiguousCommands(t *testing.T) {
	for _, command := range []string{
		"/bin/sh -c ComfyUI", "/usr/bin/python3 /opt/main.py", "/usr/bin/python3 /opt/ComfyUI/main.py --listen", "/usr/bin/python3 /opt/ComfyUI/main.py --listen 0.0.0.0", "/usr/bin/python3 /opt/ComfyUI/main.py --port 0", "/usr/bin/python3 /opt/ComfyUI/main.py --port bad", "/usr/bin/python3 /opt/ComfyUI/main.py --port 1 --port 2", "/usr/bin/python3 /opt/ComfyUI/main.py --unknown", "python3 /opt/ComfyUI/main.py", "/usr/bin/python3 '/opt/ComfyUI/main.py'",
	} {
		if _, err := inspectAutomaticLaunch([]byte("[Service]\nExecStart="+command+"\n"), "comfyui", func(string) error { return nil }); err == nil {
			t.Fatal(command)
		}
	}
	if _, err := inspectAutomaticLaunch([]byte("[Service]\nExecStart=/usr/bin/python3 /opt/ComfyUI/main.py\n"), "comfyui", func(string) error { return errors.New("untrusted") }); err == nil {
		t.Fatal("untrusted executable accepted")
	}
	for _, app := range []string{"ollama", "llama.cpp", "vllm", "unsupported"} {
		if _, err := inspectAutomaticLaunch([]byte("[Service]\nExecStart=/bin/false nope\n"), app, func(string) error { return nil }); err == nil {
			t.Fatal(app)
		}
	}
	if _, err := InspectAutomaticLaunch("/missing", "comfyui"); err == nil {
		t.Fatal("missing fragment accepted")
	}
	path := filepath.Join(t.TempDir(), "unit.service")
	os.WriteFile(path, []byte("[Service]\nExecStart=/missing /opt/ComfyUI/main.py\n"), 0600)
	if _, err := InspectAutomaticLaunch(path, "comfyui"); err == nil {
		t.Fatal("missing executable accepted")
	}
	if err := validateComfyExecutable("/missing"); err == nil {
		t.Fatal("missing executable accepted")
	}
	if err := validateComfyExecutable("/usr/bin/true"); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectAutomaticLaunch(path, "vllm"); err == nil {
		t.Fatal("missing native executable accepted")
	}
	if err := validateComfyExecutable("/usr/bin/python3"); err != nil && !strings.Contains(err.Error(), "supported") {
		t.Fatal(err)
	}
}

func comfyScriptFixture(t *testing.T) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "ComfyUI", "main.py")
	if err := os.Mkdir(filepath.Dir(script), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("# application fixture; never executed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return script
}
