package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Parse coverage for the verbatim adoption path: directives, drop-in resets,
// ComfyUI flag extraction and quote-aware ExecStart splitting.

func TestSplitExecWordsGroupsQuotedAndEscaped(t *testing.T) {
	for command, want := range map[string][]string{
		"/usr/bin/true":                                {"/usr/bin/true"},
		"/bin/sh -c \"echo a b\"":                      {"/bin/sh", "-c", "echo a b"},
		"/usr/bin/x --model '/m with space.gguf' -n 4": {"/usr/bin/x", "--model", "/m with space.gguf", "-n", "4"},
		`/usr/bin/x --tag a\"b`:                        {"/usr/bin/x", "--tag", `a"b`},
		"  /usr/bin/x\t--flag ":                        {"/usr/bin/x", "--flag"},
	} {
		got := splitExecWords(command)
		if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("split %q = %q, want %q", command, got, want)
		}
	}
}

func TestExternalLaunchParsesVerbatimDirectivesAndDropInResets(t *testing.T) {
	working := t.TempDir()
	raw := []byte("[Unit]\nDescription=d\nAfter=network-online.target\n[Service]\nType=exec\nRestart=on-failure\nRestartSec=5\nTimeoutStopSec=30\nExecStartPre=/usr/bin/test -f /etc/hostname\nExecStart=/usr/bin/ollama serve\nEnvironment=OLLAMA_NO_CLOUD=1\nEnvironment=\"OLLAMA_HOST=127.0.0.1:11434\"\nWorkingDirectory=" + working + "\n")
	unit, err := parseExternalLaunchSources([][]byte{raw}, "ollama")
	if err != nil {
		t.Fatal(err)
	}
	if unit.execStart != "/usr/bin/ollama serve" || !unit.cloudOff || unit.host != "127.0.0.1:11434" || len(unit.preCommands) != 1 {
		t.Fatalf("verbatim directives lost: %+v", unit)
	}
	reset, err := parseExternalLaunchSources([][]byte{raw, []byte("[Service]\nExecStart=\nExecStart=/usr/bin/ollama run q\nExecStartPre=\n")}, "ollama")
	if err != nil || reset.execStart != "/usr/bin/ollama run q" || len(reset.preCommands) != 0 {
		t.Fatalf("drop-in reset lost: %+v %v", reset, err)
	}
}

func TestComfyInspectExtractsEndpointAndNamesUnsafePaths(t *testing.T) {
	script := comfyScriptFixture(t)
	interpreter := filepath.Join(t.TempDir(), "python3")
	if err := os.WriteFile(interpreter, []byte("# fixture\n"), 0755); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	config := filepath.Join(t.TempDir(), "extra_model_paths.yaml")
	if err := os.WriteFile(config, []byte("comfyui: {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	command := interpreter + " " + script + " --listen 127.0.0.1 --port 8188 --output-directory " + out + " --extra-model-paths-config " + config + " --unknown-flag skipped --enable-manager"
	found, err := inspectAutomaticLaunch([]byte("[Service]\nExecStart="+command+"\n"), "comfyui", fixtureExecutableValidator)
	if err != nil || found.Endpoint != "http://127.0.0.1:8188" || found.Command != command {
		t.Fatalf("comfy parse lost flags: %+v %v", found, err)
	}
	// Quoted argument with spaces stays one word and is preserved verbatim.
	quoted := command + " --front-end-version \"1 2\""
	if found, err = inspectAutomaticLaunch([]byte("[Service]\nExecStart="+quoted+"\n"), "comfyui", fixtureExecutableValidator); err != nil || found.Command != quoted {
		t.Fatalf("quoted argument mangled: %+v %v", found, err)
	}
	if err := os.Chmod(out, 0777); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectAutomaticLaunch([]byte("[Service]\nExecStart="+command+"\n"), "comfyui", fixtureExecutableValidator); err == nil || !strings.Contains(err.Error(), out) {
		t.Fatal("world-writable output directory accepted or not named", err)
	}
}

func TestComfyConfigurationFilesAndInterpreterTrust(t *testing.T) {
	script := comfyScriptFixture(t)
	interpreter := filepath.Join(t.TempDir(), "python3")
	if err := os.WriteFile(interpreter, []byte("# fixture\n"), 0755); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "extra_model_paths.yaml")
	if err := os.WriteFile(config, []byte("comfyui: {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	command := interpreter + " " + script + " --extra-model-paths-config " + config + " --disable-auto-launch"
	if _, err := inspectAutomaticLaunch([]byte("[Service]\nExecStart="+command+"\n"), "comfyui", fixtureExecutableValidator); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(config, 0666); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectAutomaticLaunch([]byte("[Service]\nExecStart="+command+"\n"), "comfyui", fixtureExecutableValidator); err == nil || !strings.Contains(err.Error(), config) {
		t.Fatal("world-writable config accepted or not named", err)
	}
	if err := validateComfyExecutable(interpreter); err != nil {
		t.Fatal("owned interpreter rejected", err)
	}
	if err := validateComfyExecutable(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing interpreter accepted")
	}
}

func TestEnvironmentSearchPathsAndLoadedParity(t *testing.T) {
	bad := t.TempDir()
	if err := os.Chmod(bad, 0777); err != nil {
		t.Fatal(err)
	}
	if _, err := parseExternalLaunchSources([][]byte{[]byte("[Service]\nExecStart=/usr/bin/true x\nEnvironment=PATH=/usr/bin:" + bad + "\n")}, "llama.cpp"); err == nil || !strings.Contains(err.Error(), bad) {
		t.Fatal("world-writable PATH directory accepted or not named", err)
	}
	unit, err := parseExternalLaunchSources([][]byte{[]byte("[Service]\nExecStart=/usr/bin/true x\nEnvironment=PATH=/usr/bin:/usr/local/bin\nProtectHome=read-only\nPrivateTmp=true\n")}, "llama.cpp")
	if err != nil {
		t.Fatal(err)
	}
	observed := map[string]string{"ProtectHome": "read-only", "PrivateTmp": "yes"}
	if err := CheckLoadedLaunchProperties(observed, unit.loadedProperties); err != nil {
		t.Fatal(err)
	}
	observed["PrivateTmp"] = "no"
	if CheckLoadedLaunchProperties(observed, unit.loadedProperties) == nil {
		t.Fatal("loaded drift accepted")
	}
	if _, err := ValidateSystemctlExecutable("/usr/bin/true"); err != nil {
		t.Fatal(err)
	}
	if ValidateGPUInventoryExecutable(t.TempDir()+"/missing") == nil {
		t.Fatal("missing inventory executable accepted")
	}
}

func TestComfyDefaultEndpointWhenFlagsAbsent(t *testing.T) {
	script := comfyScriptFixture(t)
	interpreter := filepath.Join(t.TempDir(), "python3")
	if err := os.WriteFile(interpreter, []byte("# fixture\n"), 0755); err != nil {
		t.Fatal(err)
	}
	command := interpreter + " " + script + " --disable-auto-launch"
	found, err := inspectAutomaticLaunch([]byte("[Service]\nExecStart="+command+"\n"), "comfyui", fixtureExecutableValidator)
	if err != nil || found.Endpoint != "http://127.0.0.1:8188" {
		t.Fatalf("default endpoint lost: %+v %v", found, err)
	}
}
