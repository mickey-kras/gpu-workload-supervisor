package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
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
	pathA, pathB := t.TempDir(), t.TempDir()
	unit, err := parseExternalLaunchSources([][]byte{[]byte("[Service]\nExecStart=/usr/bin/true x\nEnvironment=PATH=" + pathA + ":" + pathB + "\nProtectHome=read-only\nPrivateTmp=true\n")}, "llama.cpp")
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

func TestComfyEndpointValidationRules(t *testing.T) {
	script := comfyScriptFixture(t)
	for _, args := range [][]string{
		{"/usr/bin/python3", script, "--listen", "127.0.0.1", "--port", "8188"},
		{"/usr/bin/python3", script},
	} {
		if _, err := comfyLaunchEndpoint(args); err != nil {
			t.Fatal("valid comfy launch rejected", err)
		}
	}
	bad := [][]string{
		{"/usr/bin/ollama", script},
		{"/usr/bin/python3", t.TempDir() + "/main.py"},
		{"/usr/bin/python3", script, "--listen", "10.0.0.5", "--port", "8188"},
		{"/usr/bin/python3", script, "--listen", "127.0.0.1", "--port", "0"},
	}
	for _, args := range bad {
		if _, err := comfyLaunchEndpoint(args); err == nil {
			t.Fatal("invalid comfy launch accepted", args)
		}
	}
}

func TestExternalServiceValueAndIsolationDirectiveValidation(t *testing.T) {
	base := "[Service]\nExecStart=/usr/bin/true x\n"
	ok := []string{"StandardOutput=journal", "StandardError=null", "KillMode=mixed", "RemainAfterExit=no", "Slice=app.slice", "KillSignal=SIGINT", "NoNewPrivileges=true", "ProtectHome=read-only", "ProtectSystem=strict", "PrivateTmp=true"}
	for _, directive := range ok {
		if _, err := parseExternalLaunchSources([][]byte{[]byte(base + directive + "\n")}, "llama.cpp"); err != nil {
			t.Fatal("compatible directive rejected:", directive, err)
		}
	}
	bad := []string{"StandardOutput=file:/x", "KillMode=process", "RemainAfterExit=yes", "Slice=x.slice", "KillSignal=SIGKILL", "NoNewPrivileges=false", "ProtectHome=maybe"}
	for _, directive := range bad {
		if _, err := parseExternalLaunchSources([][]byte{[]byte(base + directive + "\n")}, "llama.cpp"); err == nil {
			t.Fatal("incompatible directive accepted:", directive)
		}
	}
	if _, err := parseExternalLaunchSources([][]byte{[]byte("[Unit]\nWants=other.service\n" + base)}, "llama.cpp"); err == nil {
		t.Fatal("foreign unit dependency accepted")
	}
	unit, err := parseExternalLaunchSources([][]byte{[]byte(base + "KillSignal=SIGINT\nNoNewPrivileges=true\n")}, "llama.cpp")
	if err != nil {
		t.Fatal(err)
	}
	observed := map[string]string{"KillSignal": "2", "NoNewPrivileges": "yes"}
	if err := CheckLoadedLaunchProperties(observed, unit.loadedProperties); err != nil {
		t.Fatal(err)
	}
	observed["KillSignal"] = "9"
	if CheckLoadedLaunchProperties(observed, unit.loadedProperties) == nil {
		t.Fatal("kill signal drift accepted")
	}
}

func TestLaunchLineAndComfyDuplicateRejections(t *testing.T) {
	override, err := parseExternalLaunchSources([][]byte{[]byte("[Service]\nExecStart=/usr/bin/true x\nWorkingDirectory=/tmp\n"), []byte("[Service]\nWorkingDirectory=" + t.TempDir() + "\n")}, "llama.cpp")
	if err != nil || override.homeWorkingDirectory == "/tmp" {
		t.Fatal("drop-in WorkingDirectory override lost", err)
	}
	if _, err := parseExternalLaunchSources([][]byte{[]byte("[Service]\nExecStart=/usr/bin/true x\nbrokenline\n")}, "llama.cpp"); err == nil {
		t.Fatal("line without assignment accepted")
	}
	script := comfyScriptFixture(t)
	interpreter := filepath.Join(t.TempDir(), "python3")
	if err := os.WriteFile(interpreter, []byte("# fixture\n"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"--port", "--output-directory"} {
		command := interpreter + " " + script + " " + flag + " a " + flag + " b"
		if _, err := inspectAutomaticLaunch([]byte("[Service]\nExecStart="+command+"\n"), "comfyui", fixtureExecutableValidator); err == nil {
			t.Fatal("duplicate comfy option accepted:", flag)
		}
	}
}

func TestInspectQualifiedNativeLaunchFingerprintAndOwnedDropInRefusal(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "llama-server")
	if err := os.WriteFile(executable, []byte("# fixture\n"), 0755); err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(t.TempDir(), "q6_k.gguf")
	if err := os.WriteFile(model, []byte("fixture\n"), 0644); err != nil {
		t.Fatal(err)
	}
	launch := filepath.Join(t.TempDir(), "llm.service")
	raw := "[Service]\nExecStart=" + executable + " -m " + model + " --host 127.0.0.1 --port 8080\n"
	if err := os.WriteFile(launch, []byte(raw), 0644); err != nil {
		t.Fatal(err)
	}
	binding := control.NativeModel{Runtime: "llama.cpp", Endpoint: "http://127.0.0.1:8080", Model: model, LaunchFile: launch}
	fingerprint, err := inspectQualifiedNativeLaunch(launch, binding, fixtureExecutableValidator)
	if err != nil || fingerprint == "" {
		t.Fatal("qualified native launch rejected", err)
	}
	if _, err := inspectQualifiedNativeLaunch(launch, binding, func(string) error { return ErrLaunchUnsupported }); err == nil {
		t.Fatal("failed executable validation accepted")
	}
}

func TestLlamaServerOptionGrammarAndEnvironmentDuplicates(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "llama-server")
	if err := os.WriteFile(executable, []byte("# fixture\n"), 0755); err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(t.TempDir(), "q6_k.gguf")
	if err := os.WriteFile(model, []byte("fixture\n"), 0644); err != nil {
		t.Fatal(err)
	}
	valid := executable + " -m " + model + " --host 127.0.0.1 --port 8080 --ctx-size 32768 --parallel 4 -ngl 99 --alias orca"
	qualify := func(command string) error {
		unit, err := parseExternalLaunchSources([][]byte{[]byte("[Service]\nExecStart=" + command + "\n")}, "llama.cpp")
		if err != nil {
			return err
		}
		return qualifyParsedLaunch(unit, control.NativeModel{Runtime: "llama.cpp", Endpoint: "http://127.0.0.1:8080", Model: "orca"}, fixtureExecutableValidator)
	}
	if err := qualify(valid); err != nil {
		t.Fatal("supported llama.cpp options rejected", err)
	}
	for _, bad := range []string{
		"--ctx-size 0",
		"--parallel -2",
		"--flash-attn maybe",
		"--spec-type remote",
		"--unknown-flag x",
		"--model",
	} {
		if err := qualify(executable + " -m " + model + " " + bad); err == nil {
			t.Fatal("invalid llama.cpp option accepted:", bad)
		}
	}
	overrideEnv, err := parseExternalLaunchSources([][]byte{[]byte("[Service]\nExecStart=/usr/bin/true x\nEnvironment=HOME=/tmp\nEnvironment=HOME=/var\n")}, "llama.cpp")
	if err != nil || overrideEnv.environmentValues["HOME"] != "/var" {
		t.Fatal("environment override did not win", err)
	}
	ollamaUnit, err := parseExternalLaunchSources([][]byte{[]byte("[Service]\nExecStart=/usr/bin/ollama serve\nEnvironment=OLLAMA_HOST=127.0.0.1:1234\nEnvironment=OLLAMA_NO_CLOUD=1\n")}, "ollama")
	if err != nil || ollamaUnit.host != "127.0.0.1:1234" || !ollamaUnit.cloudOff {
		t.Fatal("ollama environment not bound", err)
	}
	if _, err := InspectQualifiedNativeLaunch(filepath.Join(t.TempDir(), "missing.service"), control.NativeModel{Runtime: "llama.cpp"}); err == nil {
		t.Fatal("missing launch file accepted")
	}
}

func TestOwnedUnitRenderQualifyAndParseRoundTrip(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "llama-server")
	if err := os.WriteFile(executable, []byte("# fixture\n"), 0755); err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(t.TempDir(), "q6_k.gguf")
	if err := os.WriteFile(model, []byte("fixture\n"), 0644); err != nil {
		t.Fatal(err)
	}
	owned := &control.OwnedLaunch{Executable: executable, ModelPath: model, Port: 8080, CtxSize: 4096, Alias: "orca"}
	profile := control.WorkloadProfile{ID: "owned-llama", Unit: "gpu-owned-llama.service", NativeModel: &control.NativeModel{Runtime: "llama.cpp", Endpoint: "http://127.0.0.1:8080", Model: "orca", Owned: owned}}
	if err := QualifyOwnedUnit(profile); err != nil {
		t.Fatal("rendered owned unit failed qualification", err)
	}
	raw, err := RenderOwnedUnit(profile)
	if err != nil {
		t.Fatal(err)
	}
	launch := filepath.Join(t.TempDir(), "owned.service")
	if err := os.WriteFile(launch, raw, 0644); err != nil {
		t.Fatal(err)
	}
	binding := *profile.NativeModel
	binding.LaunchFile = launch
	if _, err := inspectQualifiedNativeLaunch(launch, binding, fixtureExecutableValidator); err != nil {
		t.Fatal("owned unit parse failed", err)
	}
	binding.DropIns = []control.LaunchSource{{Path: launch}}
	if _, err := inspectQualifiedNativeLaunch(launch, binding, fixtureExecutableValidator); err == nil {
		t.Fatal("owned launch with drop-ins accepted")
	}
	missing := *owned
	missing.Executable = filepath.Join(t.TempDir(), "llama-server")
	profile.NativeModel.Owned = &missing
	if QualifyOwnedUnit(profile) == nil {
		t.Fatal("missing owned executable qualified")
	}
}

func TestOwnedOllamaEnvironmentAndHomeSpecifier(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "ollama")
	if err := os.WriteFile(executable, []byte("# fixture\n"), 0755); err != nil {
		t.Fatal(err)
	}
	owned := &control.OwnedLaunch{Executable: executable, Port: 11434}
	profile := control.WorkloadProfile{ID: "owned-ollama", Unit: "gpu-owned-ollama.service", NativeModel: &control.NativeModel{Runtime: "ollama", Instance: "inst1", Endpoint: "http://127.0.0.1:11434", Owned: owned}}
	raw, err := RenderOwnedUnit(profile)
	if err != nil {
		t.Fatal(err)
	}
	launch := filepath.Join(t.TempDir(), "owned-ollama.service")
	if err := os.WriteFile(launch, raw, 0644); err != nil {
		t.Fatal(err)
	}
	binding := *profile.NativeModel
	binding.LaunchFile = launch
	if _, err := inspectQualifiedNativeLaunch(launch, binding, fixtureExecutableValidator); err != nil {
		t.Fatal("owned ollama parse failed", err)
	}
	expectedHome, homeErr := localAccountHome()
	if homeErr != nil {
		t.Skip("no local passwd record in sandbox")
	}
	unit, err := parseExternalLaunchSources([][]byte{[]byte("[Service]\nExecStart=/usr/bin/true x\nWorkingDirectory=%h\n")}, "llama.cpp")
	if err != nil || unit.homeWorkingDirectory != expectedHome {
		t.Fatalf("home specifier not expanded: %q vs %q (%v)", unit.homeWorkingDirectory, expectedHome, err)
	}
}

func TestDropInDigestOrderingAndPreCommandLimits(t *testing.T) {
	dropIn := filepath.Join(t.TempDir(), "override.conf")
	if err := os.WriteFile(dropIn, []byte("[Service]\nRestart=no\n"), 0644); err != nil {
		t.Fatal(err)
	}
	stale := []control.LaunchSource{{Path: dropIn, SHA256: "0000000000000000000000000000000000000000000000000000000000000000"}}
	if _, err := readLaunchSources(filepath.Join(t.TempDir(), "missing.service"), stale); err == nil {
		t.Fatal("missing unit with drop-in accepted")
	}
	main := filepath.Join(t.TempDir(), "main.service")
	if err := os.WriteFile(main, []byte("[Service]\nExecStart=/usr/bin/true x\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readLaunchSources(main, stale); err == nil {
		t.Fatal("stale drop-in digest accepted")
	}
	if _, err := parseExternalLaunchSources([][]byte{[]byte("[Unit]\nAfter=network-online.target\n[Service]\nExecStart=/usr/bin/true x\n")}, "llama.cpp"); err != nil {
		t.Fatal("valid ordering rejected", err)
	}
	if _, err := parseExternalLaunchSources([][]byte{[]byte("[Unit]\nAfter=not a unit\n[Service]\nExecStart=/usr/bin/true x\n")}, "llama.cpp"); err == nil {
		t.Fatal("invalid ordering accepted")
	}
	pre := "[Service]\n"
	for i := 0; i < 33; i++ {
		pre += "ExecStartPre=/usr/bin/true\n"
	}
	if _, err := parseExternalLaunchSources([][]byte{[]byte(pre + "ExecStart=/usr/bin/true x\n")}, "llama.cpp"); err == nil {
		t.Fatal("unbounded ExecStartPre accepted")
	}
	if _, err := parseExternalLaunchSources([][]byte{[]byte("[Service]\nExecStart=/usr/bin/true x\nEnvironment=LD_LIBRARY_PATH=/usr/lib\nTimeoutStartSec=abc\n")}, "llama.cpp"); err == nil {
		t.Fatal("non-numeric timeout accepted")
	}
}

func TestVLLMServeGrammarAndModelDirectoryType(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "vllm")
	if err := os.WriteFile(executable, []byte("# fixture\n"), 0755); err != nil {
		t.Fatal(err)
	}
	modelDir := t.TempDir()
	command := executable + " serve " + modelDir + " --host 127.0.0.1 --port 8000 --served-model-name q"
	qualify := func(cmd string) error {
		unit, err := parseExternalLaunchSources([][]byte{[]byte("[Service]\nExecStart=" + cmd + "\n")}, "vllm")
		if err != nil {
			return err
		}
		return qualifyParsedLaunch(unit, control.NativeModel{Runtime: "vllm", Endpoint: "http://127.0.0.1:8000", Model: "q"}, fixtureExecutableValidator)
	}
	if err := qualify(command); err != nil {
		t.Fatal("vllm serve grammar rejected", err)
	}
	if err := qualify(executable + " serve " + modelDir + "/missing.gguf --host 127.0.0.1 --port 8000"); err == nil {
		t.Fatal("vllm file-as-model accepted")
	}
}

func sha256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func TestAutomaticInspectWithDropInsAndGrammarTails(t *testing.T) {
	script := comfyScriptFixture(t)
	interpreter := filepath.Join(t.TempDir(), "python3")
	if err := os.WriteFile(interpreter, []byte("# fixture\n"), 0755); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	main := filepath.Join(t.TempDir(), "comfy.service")
	command := interpreter + " " + script + " --output-directory " + out + " --disable-auto-launch --disable-auto-launch"
	if err := os.WriteFile(main, []byte("[Service]\nExecStart="+command+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectAutomaticLaunchSourcesWithValidator(main, "comfyui", nil, fixtureExecutableValidator); err == nil {
		t.Fatal("duplicate comfy flag accepted")
	}
	dropIn := filepath.Join(t.TempDir(), "extra.conf")
	dropRaw := []byte("[Service]\nTimeoutStopSec=30\n")
	if err := os.WriteFile(dropIn, dropRaw, 0644); err != nil {
		t.Fatal(err)
	}
	digest := sha256Hex(dropRaw)
	single := filepath.Join(t.TempDir(), "single.service")
	if err := os.WriteFile(single, []byte("[Service]\nExecStart="+interpreter+" "+script+" --disable-auto-launch\n"), 0644); err != nil {
		t.Fatal(err)
	}
	found, err := inspectAutomaticLaunchSourcesWithValidator(single, "comfyui", []string{dropIn}, fixtureExecutableValidator)
	if err != nil || len(found.DropIns) != 1 || found.DropIns[0].SHA256 != digest {
		t.Fatal("drop-in digest not recorded", err)
	}
	if _, err := inspectAutomaticLaunchSourcesWithValidator(filepath.Join(t.TempDir(), "none.service"), "comfyui", nil, fixtureExecutableValidator); err == nil {
		t.Fatal("missing unit accepted")
	}
	if _, err := parseExternalLaunchSources([][]byte{[]byte("ExecStart=/usr/bin/true x\n")}, "llama.cpp"); err == nil {
		t.Fatal("directive outside section accepted")
	}
	if _, err := parseExternalLaunchSources([][]byte{[]byte("[Service]\nExecStart=/usr/bin/true x\nEnvironment=PATH=relative/bin\n")}, "llama.cpp"); err == nil {
		t.Fatal("relative PATH directory accepted")
	}
	executable := filepath.Join(t.TempDir(), "llama-server")
	if err := os.WriteFile(executable, []byte("# fixture\n"), 0755); err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(t.TempDir(), "q6_k.gguf")
	if err := os.WriteFile(model, []byte("fixture\n"), 0644); err != nil {
		t.Fatal(err)
	}
	aliases := executable + " -m " + model + " --host 127.0.0.1 --port 8080 -c 4096 -np 2 -ngl 5 -cb -fa on --alias orca"
	unit, err := parseExternalLaunchSources([][]byte{[]byte("[Service]\nExecStart=" + aliases + "\n")}, "llama.cpp")
	if err != nil {
		t.Fatal("llama flag aliases rejected", err)
	}
	if err := qualifyParsedLaunch(unit, control.NativeModel{Runtime: "llama.cpp", Endpoint: "http://127.0.0.1:8080", Model: "orca"}, fixtureExecutableValidator); err != nil {
		t.Fatal("llama flag aliases failed qualification", err)
	}
}

func TestOwnedSpecVerificationAndRemainingGrammarBranches(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "llama-server")
	if err := os.WriteFile(executable, []byte("# fixture\n"), 0755); err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(t.TempDir(), "q6_k.gguf")
	if err := os.WriteFile(model, []byte("fixture\n"), 0644); err != nil {
		t.Fatal(err)
	}
	owned := &control.OwnedLaunch{Executable: executable, ModelPath: model, Port: 8080, Alias: "orca"}
	profile := control.WorkloadProfile{ID: "owned-llama", Unit: "gpu-owned-llama.service", NativeModel: &control.NativeModel{Runtime: "llama.cpp", Endpoint: "http://127.0.0.1:8080", Model: "orca", Owned: owned}}
	raw, err := RenderOwnedUnit(profile)
	if err != nil {
		t.Fatal(err)
	}
	launch := filepath.Join(t.TempDir(), "owned.service")
	if err := os.WriteFile(launch, raw, 0644); err != nil {
		t.Fatal(err)
	}
	profile.NativeModel.LaunchFile = launch
	profile.NativeModel.LaunchSHA256 = sha256Hex(raw)
	if err := verifyOwnedSpec(profile); err != nil {
		t.Fatal("rendered spec failed verification", err)
	}
	profile.NativeModel.LaunchSHA256 = "0"
	if verifyOwnedSpec(profile) == nil {
		t.Fatal("stale spec digest accepted")
	}
	script := comfyScriptFixture(t)
	interpreter := filepath.Join(t.TempDir(), "python3")
	if err := os.WriteFile(interpreter, []byte("# fixture\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := comfyLaunchEndpointMode([]string{interpreter, script, "--disable-auto-launch"}, true); err != nil {
		t.Fatal("cleanup mode endpoint rejected", err)
	}
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("hf_x\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := parseExternalLaunchSources([][]byte{[]byte("[Service]\nExecStart=/usr/bin/true x\nEnvironment=HF_TOKEN_PATH=" + token + "\n")}, "llama.cpp"); err != nil {
		t.Fatal("token file rejected", err)
	}
	many := interpreter + " " + script + " --extra-model-paths-config"
	for i := 0; i < 33; i++ {
		many += " " + token
	}
	if _, err := inspectAutomaticLaunch([]byte("[Service]\nExecStart="+many+"\n"), "comfyui", fixtureExecutableValidator); err == nil {
		t.Fatal("unbounded configuration file count accepted")
	}
	if _, err := parseExternalLaunchSources([][]byte{[]byte("[Service]\nExecStart=/usr/bin/true x\nLimitMEMLOCK=notanumber\n")}, "llama.cpp"); err == nil {
		t.Fatal("non-numeric limit accepted")
	}
	longKey := "[Service]\nExecStart=/usr/bin/true x\n" + strings.Repeat("K", 70) + "=v\n"
	if _, err := parseExternalLaunchSources([][]byte{[]byte(longKey)}, "llama.cpp"); err == nil {
		t.Fatal("oversized directive key accepted")
	}
	if _, err := parseExternalLaunchSources([][]byte{[]byte("[Service]\nExecStart=/usr/bin/true x\nStandardError=inherit\n")}, "llama.cpp"); err != nil {
		t.Fatal("inherit standard error rejected", err)
	}
	modelDir := t.TempDir()
	dirAsModel := executable + " -m " + modelDir + " --host 127.0.0.1 --port 8080"
	unit, err := parseExternalLaunchSources([][]byte{[]byte("[Service]\nExecStart=" + dirAsModel + "\n")}, "llama.cpp")
	if err != nil {
		t.Fatal(err)
	}
	if err := qualifyParsedLaunch(unit, control.NativeModel{Runtime: "llama.cpp", Endpoint: "http://127.0.0.1:8080", Model: modelDir}, fixtureExecutableValidator); err == nil {
		t.Fatal("directory accepted as llama.cpp model file")
	}
}

func TestDropInPathRulesAndEnvironmentResetBranches(t *testing.T) {
	script := comfyScriptFixture(t)
	interpreter := filepath.Join(t.TempDir(), "python3")
	if err := os.WriteFile(interpreter, []byte("# fixture\n"), 0755); err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(t.TempDir(), "comfy.service")
	if err := os.WriteFile(main, []byte("[Service]\nExecStart="+interpreter+" "+script+" --disable-auto-launch\n"), 0644); err != nil {
		t.Fatal(err)
	}
	notConf := filepath.Join(t.TempDir(), "extra.txt")
	if err := os.WriteFile(notConf, []byte("[Service]\nTimeoutStopSec=30\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectAutomaticLaunchSourcesWithValidator(main, "comfyui", []string{notConf}, fixtureExecutableValidator); err == nil {
		t.Fatal("non-.conf drop-in accepted")
	}
	if _, err := inspectAutomaticLaunchSourcesWithValidator(main, "comfyui", []string{notConf, notConf}, fixtureExecutableValidator); err == nil {
		t.Fatal("duplicate drop-in accepted")
	}
	many := make([]string, 33)
	for i := range many {
		many[i] = notConf
	}
	if _, err := inspectAutomaticLaunchSourcesWithValidator(main, "comfyui", many, fixtureExecutableValidator); err == nil {
		t.Fatal("unbounded drop-in count accepted")
	}
	if _, err := parseExternalLaunchSources([][]byte{[]byte("[Service]\nExecStart=/usr/bin/true x\nExecStart=/usr/bin/true y\n")}, "llama.cpp"); err == nil {
		t.Fatal("duplicate ExecStart in one source accepted")
	}
	reset, err := parseExternalLaunchSources([][]byte{[]byte("[Service]\nExecStart=/usr/bin/true x\nExecStart=\nExecStart=/usr/bin/true y\n")}, "llama.cpp")
	if err != nil || reset.execStart != "/usr/bin/true y" {
		t.Fatal("exec reset pattern rejected", err)
	}
	emptyEnv, err := parseExternalLaunchSources([][]byte{[]byte("[Service]\nExecStart=/usr/bin/true x\nEnvironment=HOME=/tmp\nEnvironment=\n")}, "llama.cpp")
	if err != nil || len(emptyEnv.environmentValues) != 0 {
		t.Fatal("environment reset lost assignments incorrectly", err)
	}
	if _, err := parseExternalLaunchSources([][]byte{[]byte("[Service]\nExecStart=/usr/bin/true x\nEnvironment=\"HOME=/tmp\n")}, "llama.cpp"); err == nil {
		t.Fatal("unterminated environment quote accepted")
	}
}

func TestUnitMetadataAndTimingDirectives(t *testing.T) {
	raw := "[Unit]\nDescription=example workload\n[Service]\nExecStart=/usr/bin/true x\nTimeoutStopSec=30\nRestartSec=5\n[Install]\nWantedBy=default.target\n"
	if _, err := parseExternalLaunchSources([][]byte{[]byte(raw)}, "llama.cpp"); err != nil {
		t.Fatal("unit metadata and timing directives rejected", err)
	}
	executable := filepath.Join(t.TempDir(), "llama-server")
	if err := os.WriteFile(executable, []byte("# fixture\n"), 0755); err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(t.TempDir(), "q6_k.gguf")
	if err := os.WriteFile(model, []byte("fixture\n"), 0644); err != nil {
		t.Fatal(err)
	}
	mtp := executable + " -m " + model + " --host 127.0.0.1 --port 8080 --spec-type draft-mtp --flash-attn auto"
	unit, err := parseExternalLaunchSources([][]byte{[]byte("[Service]\nExecStart=" + mtp + "\n")}, "llama.cpp")
	if err != nil {
		t.Fatal(err)
	}
	if err := qualifyParsedLaunch(unit, control.NativeModel{Runtime: "llama.cpp", Endpoint: "http://127.0.0.1:8080", Model: model}, fixtureExecutableValidator); err != nil {
		t.Fatal("MTP spec rejected", err)
	}
	script := comfyScriptFixture(t)
	interpreter := filepath.Join(t.TempDir(), "python3")
	if err := os.WriteFile(interpreter, []byte("# fixture\n"), 0755); err != nil {
		t.Fatal(err)
	}
	dup := interpreter + " " + script + " --enable-manager --enable-manager"
	if _, err := inspectAutomaticLaunch([]byte("[Service]\nExecStart="+dup+"\n"), "comfyui", fixtureExecutableValidator); err == nil {
		t.Fatal("duplicate --enable-manager accepted")
	}
}

func TestIndependentBlockersAndDropInReadFailures(t *testing.T) {
	script := comfyScriptFixture(t)
	interpreter := filepath.Join(t.TempDir(), "python3")
	if err := os.WriteFile(interpreter, []byte("# fixture\n"), 0755); err != nil {
		t.Fatal(err)
	}
	bad := t.TempDir()
	if err := os.Chmod(bad, 0777); err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(t.TempDir(), "comfy.service")
	if err := os.WriteFile(main, []byte("[Service]\nExecStart="+interpreter+" "+script+" --disable-auto-launch\nEnvironment=PATH="+bad+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectAutomaticLaunchSourcesWithValidator(main, "comfyui", nil, fixtureExecutableValidator); err == nil || !strings.Contains(err.Error(), bad) {
		t.Fatal("independent environment blocker lost", err)
	}
	dirDropIn := filepath.Join(t.TempDir(), "broken.conf")
	if err := os.Mkdir(dirDropIn, 0755); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(t.TempDir(), "ok.service")
	if err := os.WriteFile(good, []byte("[Service]\nExecStart="+interpreter+" "+script+" --disable-auto-launch\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectAutomaticLaunchSourcesWithValidator(good, "comfyui", []string{dirDropIn}, fixtureExecutableValidator); err == nil {
		t.Fatal("directory drop-in accepted")
	}
	if _, err := inspectAutomaticLaunch([]byte("[Service]\nExecStart="+interpreter+" "+script+" --output-directory\n"), "comfyui", fixtureExecutableValidator); err == nil {
		t.Fatal("directory option without value accepted")
	}
	dirA, dirB := t.TempDir(), t.TempDir()
	if _, err := parseExternalLaunchSources([][]byte{[]byte("[Service]\nExecStart=/usr/bin/true x\nEnvironment=PATH=" + dirA + "::" + dirB + "\n")}, "llama.cpp"); err == nil {
		t.Fatal("empty PATH entry accepted")
	}
	executable := filepath.Join(t.TempDir(), "llama-server")
	if err := os.WriteFile(executable, []byte("# fixture\n"), 0755); err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(t.TempDir(), "q6_k.gguf")
	if err := os.WriteFile(model, []byte("fixture\n"), 0644); err != nil {
		t.Fatal(err)
	}
	owned := &control.OwnedLaunch{Executable: executable, ModelPath: model, Port: 8080, Alias: "orca", Conflicts: control.OwnedUnitFilePrefix + "peer.service"}
	profile := control.WorkloadProfile{ID: "owned-llama", Unit: "gpu-owned-llama.service", NativeModel: &control.NativeModel{Runtime: "llama.cpp", Endpoint: "http://127.0.0.1:8080", Model: "orca", Owned: owned}}
	if err := QualifyOwnedUnit(profile); err != nil {
		t.Fatal("owned unit with peers failed qualification", err)
	}
	stripped := []byte("[Unit]\nDescription=Supervisor-owned llama.cpp launch for owned-llama\n[Service]\nType=simple\nRestart=no\nExecStart=" + executable + " -m " + model + " --host 127.0.0.1 --port 8080 --alias orca\n")
	if _, err := parseOwnedLaunchUnit(stripped, *profile.NativeModel); err == nil {
		t.Fatal("owned unit without rendered peer dependencies accepted")
	}
}
