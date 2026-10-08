package setup

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestApplicationDiscoveryFiltersServicesByExecutable(t *testing.T) {
	backend, home, _ := fixture(t)
	backend.runCommand = func(ctx context.Context, exe string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if exe != "/usr/bin/systemctl" {
			t.Fatalf("unexpected executable %s", exe)
		}
		if strings.Contains(joined, "list-unit-files") {
			return []byte("ollama.service disabled\nvllm-fake.service enabled\nssh.service enabled\ncustom.service enabled\n"), nil
		}
		if strings.HasSuffix(joined, "-- ollama.service") {
			return []byte("ExecStart={ path=/usr/bin/ollama ; argv[]=/usr/bin/ollama serve ; ignore_errors=no ; }\nControlGroup=/user.slice/ollama.service\nActiveState=inactive\nSubState=dead\n"), nil
		}
		if strings.HasSuffix(joined, "-- vllm-fake.service") {
			return []byte("ExecStart={ path=/usr/bin/other ; argv[]=/usr/bin/other ; }\nActiveState=active\n"), nil
		}
		return []byte("ExecStart={ path=/usr/bin/other ; argv[]=/usr/bin/other ; }"), nil
	}
	backend.probeApplication = func(ctx context.Context, r ProbeRequest) (ApplicationCandidate, error) { return candidate(r), nil }
	got, err := backend.Discover(context.Background(), home)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Units) != 1 || got.Units[0] != "ollama.service" {
		t.Fatalf("unrelated services leaked: %v", got.Units)
	}
	if len(got.Applications) != 6 {
		t.Fatal(got.Applications)
	}
	service := got.Applications[4]
	if service.App != "ollama" || service.InstanceStatus != "not-running" || service.Cgroup != "/user.slice/ollama.service" || service.LifecycleControl != "unverified" {
		t.Fatalf("%+v", service)
	}
	data, err := json.Marshal(service)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"models":[]`) {
		t.Fatalf("unit candidate must report an empty model list, not null: %s", data)
	}
}
func TestServiceIdentityDoesNotExecuteConfiguration(t *testing.T) {
	for _, input := range []string{"ExecStart={ path=/bin/bash ; argv[]=/bin/bash -c ollama serve ; }", "ExecStart={ path=/usr/bin/python ; argv[]=/usr/bin/python /tmp/main.py ; }", "ExecStart={ path=/usr/bin/not-ollama ; argv[]=/usr/bin/not-ollama ; }", "ExecStart={ path=llama-server ; argv[]=llama-server -m /a ; }", "ExecStart={ path=/usr/bin/llama-server ; argv[]=/usr/bin/other -m /a ; }", "ExecStart={ path=/usr/bin/llama-server ; argv[]=/usr/bin/llama-server -m /a ; } { path=/usr/bin/other ; argv[]=/usr/bin/other x ; }"} {
		if appFromUnit(input) != "" {
			t.Fatal("guessed application", input)
		}
	}
	for _, tt := range []struct{ input, app string }{{"ExecStart={ path=/usr/bin/llama-server ; argv[]=/usr/bin/llama-server -m /a.gguf ; }", "llama.cpp"}, {"ExecStart={ path=/usr/bin/vllm ; argv[]=/usr/bin/vllm serve /a ; }", "vllm"}, {"ExecStart={ path=/opt/env/bin/python3 ; argv[]=/opt/env/bin/python3 -m vllm.entrypoints.openai.api_server ; }", "vllm"}, {"ExecStart={ path=/opt/env/bin/python ; argv[]=/opt/env/bin/python /opt/ComfyUI/main.py ; }", "comfyui"}} {
		if got := appFromUnit(tt.input); got != tt.app {
			t.Fatalf("%s != %s", got, tt.app)
		}
	}
}

func TestDiscoveryUsesLaunchReferencesWithoutExecutingThem(t *testing.T) {
	input := "ExecStart={ path=/usr/bin/llama-server ; argv[]=/usr/bin/llama-server -m /models/a.gguf --alias chat ; }\n"
	found := launchModels("llama.cpp", input)
	if len(found) != 1 || found[0].ID != "/models/a.gguf" || found[0].Loaded != "unknown" {
		t.Fatal(found)
	}
	input = "ExecStart={ path=/usr/bin/vllm ; argv[]=/usr/bin/vllm serve org/model --served-model-name one two --port 8000 ; }\n"
	found = launchModels("vllm", input)
	if len(found) != 1 || found[0].ID != "org/model" || len(found[0].Aliases) != 2 || found[0].Locality != "unknown" {
		t.Fatal(found)
	}
	if found := launchModels("comfyui", input); len(found) != 0 {
		t.Fatal("ComfyUI model picker")
	}
	if found := launchModels("llama.cpp", "ExecStart={ path=/bin/sh ; argv[]=/bin/sh -c 'llama-server -m /model' ; }"); len(found) != 0 {
		t.Fatal("shell command parsed")
	}
}
