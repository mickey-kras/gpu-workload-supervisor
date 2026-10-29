package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/vllmkey"
)

func hardenedQualification(t *testing.T, lines, args string, n control.NativeModel) error {
	t.Helper()
	model := filepath.Join(t.TempDir(), "model")
	if err := os.Mkdir(model, 0o700); err != nil {
		t.Fatal(err)
	}
	unit, err := parseExternalLaunchSources([][]byte{[]byte("[Service]\n" + lines + "ExecStart=/usr/bin/vllm serve " + model + " --served-model-name selected --port 9000" + args + "\n")})
	if err != nil {
		return err
	}
	return qualifyParsedMainCommand(unit, n, func(string) error { return nil })
}

func TestHardenedServingAcceptsEveryRequiredSetting(t *testing.T) {
	h := newHardenedFixture(t)
	n := h.bind(control.NativeModel{Runtime: "vllm", Model: "selected", Endpoint: "http://127.0.0.1:9000"})
	if err := hardenedQualification(t, h.lines(), " --host 127.0.0.1"+h.args(), n); err != nil {
		t.Fatal(err)
	}
}

func TestHardenedServingRejectsEachMissingSetting(t *testing.T) {
	h := newHardenedFixture(t)
	n := h.bind(control.NativeModel{Runtime: "vllm", Model: "selected", Endpoint: "http://127.0.0.1:9000"})
	other := newHardenedFixture(t)
	changedTemplate := newHardenedFixture(t)
	if err := os.WriteFile(changedTemplate.template, []byte("{{ changed }}"), 0o644); err != nil {
		t.Fatal(err)
	}
	badKey := newHardenedFixture(t)
	if err := os.Chmod(badKey.keyFile, 0o644); err != nil {
		t.Fatal(err)
	}
	host := " --host 127.0.0.1"
	offline := "Environment=HF_HUB_OFFLINE=1\n"
	keyFile := "EnvironmentFile=" + h.keyFile + "\n"
	format := " --load-format safetensors"
	template := " --chat-template " + h.template
	cases := []struct {
		name, lines, args, want string
		binding                 control.NativeModel
	}{
		{"wildcard listener", offline + keyFile, " --host 0.0.0.0" + format + template, "--host 127.0.0.1", n},
		{"no load format", offline + keyFile, host + template, "--load-format safetensors", n},
		{"no chat template", offline + keyFile, host + format, "--chat-template", n},
		{"other chat template", offline + keyFile, host + format + " --chat-template " + other.template, "not the one that setup recorded", n},
		{"changed chat template", offline + keyFile, host + format + " --chat-template " + changedTemplate.template, "content changed", changedTemplate.bind(n)},
		{"online hub", keyFile, host + format + template, "HF_HUB_OFFLINE=1", n},
		{"online hub value", "Environment=HF_HUB_OFFLINE=0\n" + keyFile, host + format + template, "HF_HUB_OFFLINE=1", n},
		{"no key file", offline, host + format + template, "EnvironmentFile", n},
		{"other key file", offline + "EnvironmentFile=" + other.keyFile + "\n", host + format + template, "API key file is not the one", n},
		{"unsafe key file", offline + "EnvironmentFile=" + badKey.keyFile + "\n", host + format + template, "0600", badKey.bind(n)},
		{"unbound binding", offline + keyFile, host + format + template, "not the one that setup recorded", control.NativeModel{Runtime: "vllm", Model: "selected", Endpoint: "http://127.0.0.1:9000"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "wildcard listener" {
				tc.binding.Endpoint = "http://0.0.0.0:9000"
			}
			err := hardenedQualification(t, tc.lines, tc.args, tc.binding)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), strings.Repeat("k", 43)) {
				t.Fatal("error contains the API key")
			}
		})
	}
}

func TestHardenedServingRejectsUnsafeUnitSettings(t *testing.T) {
	h := newHardenedFixture(t)
	for name, source := range map[string]string{
		"inline key":        "[Service]\nEnvironment=VLLM_API_KEY=" + strings.Repeat("k", 43) + "\n",
		"runtime adapters":  "[Service]\nEnvironment=VLLM_ALLOW_RUNTIME_LORA_UPDATING=True\n",
		"dev mode":          "[Service]\nEnvironment=VLLM_SERVER_DEV_MODE=1\n",
		"insecure input":    "[Service]\nEnvironment=VLLM_ALLOW_INSECURE_SERIALIZATION=1\n",
		"plugins":           "[Service]\nEnvironment=VLLM_PLUGINS=example\n",
		"unknown setting":   "[Service]\nEnvironment=VLLM_FUTURE_SWITCH=1\n",
		"relative key file": "[Service]\nEnvironmentFile=vllm.env\n",
		"optional key file": "[Service]\nEnvironmentFile=-" + h.keyFile + "\n",
		"expanded key file": "[Service]\nEnvironmentFile=%h/vllm.env\n",
		"two key files":     "[Service]\nEnvironmentFile=" + h.keyFile + "\nEnvironmentFile=" + h.keyFile + "\n",
		"reset key files":   "[Service]\nEnvironmentFile=\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseExternalLaunchSources([][]byte{[]byte(source + "ExecStart=/usr/bin/true x\n")})
			if !errors.Is(err, ErrLaunchUnsupported) {
				t.Fatalf("error = %v", err)
			}
			if strings.Contains(fmt.Sprint(err), strings.Repeat("k", 43)) {
				t.Fatal("error contains the API key")
			}
		})
	}
	if _, err := parseLaunchUnit([]byte("[Service]\nEnvironmentFile=relative\n")); !errors.Is(err, ErrLaunchUnsupported) {
		t.Fatalf("owned grammar accepted a relative EnvironmentFile: %v", err)
	}
	if _, err := parseExternalLaunchSources([][]byte{[]byte("[Service]\nEnvironmentFile=" + h.keyFile + "\n"), []byte("[Service]\nEnvironmentFile=" + h.keyFile + "\n")}); !errors.Is(err, ErrLaunchUnsupported) {
		t.Fatalf("a drop-in added a second EnvironmentFile: %v", err)
	}
}

func TestComfyLaunchRejectsEnvironmentFile(t *testing.T) {
	script := comfyScriptFixture(t)
	h := newHardenedFixture(t)
	raw := []byte("[Service]\nEnvironmentFile=" + h.keyFile + "\nExecStart=/usr/bin/python3 " + script + "\n")
	if _, err := inspectAutomaticLaunch(raw, "comfyui", func(string) error { return nil }); !errors.Is(err, ErrLaunchUnsupported) {
		t.Fatalf("ComfyUI EnvironmentFile accepted: %v", err)
	}
}

func TestCheckLoadedEnvironmentFiles(t *testing.T) {
	if err := CheckLoadedEnvironmentFiles("", ""); err != nil {
		t.Fatal(err)
	}
	if err := CheckLoadedEnvironmentFiles("/k/vllm.env (ignore_errors=no)\n", "/k/vllm.env"); err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{"", "/k/vllm.env (ignore_errors=yes)", "/k/vllm.env (ignore_errors=no) /k/other (ignore_errors=no)", "/k/other (ignore_errors=no)"} {
		if CheckLoadedEnvironmentFiles(output, "/k/vllm.env") == nil {
			t.Fatalf("loaded EnvironmentFiles %q accepted", output)
		}
	}
	if CheckLoadedEnvironmentFiles("/k/vllm.env (ignore_errors=no)", "") == nil {
		t.Fatal("uninspected EnvironmentFile accepted")
	}
}

func TestParseServingVersion(t *testing.T) {
	cases := []struct {
		text    string
		release bool
		version servingVersion
	}{
		{"0.14.1", true, servingVersion{0, 14, 1}},
		{"v0.27.0", true, servingVersion{0, 27, 0}},
		{"0.14.1+cu129", true, servingVersion{0, 14, 1}},
		{"0.14.1.post1", true, servingVersion{0, 14, 1}},
		{"0.14.1rc1", false, servingVersion{0, 14, 1}},
		{"0.14.1.dev5+gabc", false, servingVersion{0, 14, 1}},
		{"0.14.1.1", true, servingVersion{0, 14, 1}},
		{"0.14.1.1rc1", false, servingVersion{0, 14, 1}},
	}
	for _, tc := range cases {
		version, release, err := parseServingVersion(tc.text)
		if err != nil || version != tc.version || release != tc.release {
			t.Fatalf("%s = %v %v %v", tc.text, version, release, err)
		}
	}
	for _, text := range []string{"", "0.14", "latest", "99999999999.0.0", strings.Repeat("1", 129) + ".0.0"} {
		if _, _, err := parseServingVersion(text); !errors.Is(err, ErrServingVersion) {
			t.Fatalf("%q accepted: %v", text, err)
		}
	}
	if (servingVersion{0, 14, 1}).String() != "0.14.1" {
		t.Fatal("version text")
	}
}

func TestCheckServingVersionFloors(t *testing.T) {
	for _, tc := range []struct {
		version string
		nvfp4   bool
		ok      bool
	}{
		{"0.14.1", false, true},
		{"0.14.1rc2", false, false},
		{"0.14.0", false, false},
		{"0.13.9", false, false},
		{"1.0.0", false, true},
		{"0.26.9", true, false},
		{"0.27.0rc1", true, false},
		{"0.27.0", true, true},
		{"bad", false, false},
	} {
		err := compareServingVersion(tc.version, tc.nvfp4)
		if (err == nil) != tc.ok {
			t.Fatalf("%s nvfp4=%v: %v", tc.version, tc.nvfp4, err)
		}
		if err != nil && !errors.Is(err, ErrServingVersion) {
			t.Fatalf("%s: %v", tc.version, err)
		}
	}
}

func TestModelUsesNVFP4(t *testing.T) {
	write := func(t *testing.T, files map[string]string) string {
		dir := t.TempDir()
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	for name, tc := range map[string]struct {
		files map[string]string
		nvfp4 bool
	}{
		"no metadata":     {map[string]string{}, false},
		"bf16":            {map[string]string{"config.json": `{"torch_dtype":"bfloat16"}`}, false},
		"fp8":             {map[string]string{"config.json": `{"quantization_config":{"quant_method":"fp8"}}`}, false},
		"compressed fp4":  {map[string]string{"config.json": `{"quantization_config":{"format":"nvfp4-pack-quantized"}}`}, true},
		"mxfp4":           {map[string]string{"config.json": `{"quantization_config":{"quant_method":"mxfp4"}}`}, false},
		"modelopt fp4":    {map[string]string{"config.json": `{"quantization_config":{"quant_method":"modelopt_fp4"}}`}, true},
		"hf quant config": {map[string]string{"config.json": `{}`, "hf_quant_config.json": `{"quantization":{"quant_algo":"NVFP4"}}`}, true},
		"fp4 elsewhere":   {map[string]string{"config.json": `{"name":"fp4-notes"}`}, false},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := modelUsesNVFP4(write(t, tc.files))
			if err != nil || got != tc.nvfp4 {
				t.Fatalf("nvfp4 = %v, %v", got, err)
			}
		})
	}
	if _, err := modelUsesNVFP4(write(t, map[string]string{"config.json": "{"})); !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("invalid config accepted: %v", err)
	}
	if _, err := modelUsesNVFP4(write(t, map[string]string{"config.json": strings.Repeat(" ", maxModelMetaBytes+1)})); !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("oversized config accepted: %v", err)
	}
	unreadable := t.TempDir()
	if err := os.Mkdir(filepath.Join(unreadable, "config.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	fifo := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(fifo, "config.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := modelUsesNVFP4(fifo); !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("FIFO config accepted: %v", err)
	}
	if _, err := modelUsesNVFP4(unreadable); !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("directory config accepted: %v", err)
	}
}

func TestChatTemplateSHA256Failures(t *testing.T) {
	if _, err := ChatTemplateSHA256("relative.jinja"); !errors.Is(err, ErrLaunchUnsupported) {
		t.Fatalf("relative template accepted: %v", err)
	}
	large := filepath.Join(t.TempDir(), "large.jinja")
	if err := os.WriteFile(large, make([]byte, maxTemplateBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ChatTemplateSHA256(large); !errors.Is(err, ErrLaunchUnsupported) {
		t.Fatalf("oversized template accepted: %v", err)
	}
}

func TestServingVersionReadiness(t *testing.T) {
	version := `{"version":"0.14.1"}`
	status := http.StatusOK
	server := httptest.NewServer(servingAPI(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"selected"}]}`)
	}))
	defer server.Close()
	versioned := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			w.WriteHeader(status)
			fmt.Fprint(w, version)
			return
		}
		server.Config.Handler.ServeHTTP(w, r)
	}))
	defer versioned.Close()
	m := &SystemdManager{client: versioned.Client()}
	n := servingBinding(t, versioned.URL, "selected")
	if err := m.nativeReady(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		body      string
		status    int
		permanent bool
	}{
		{`{"version":"0.14.0"}`, http.StatusOK, true},
		{`{"version":`, http.StatusOK, true},
		{strings.Repeat(" ", 4097), http.StatusOK, true},
		{`{}`, http.StatusNotFound, true},
		{`{}`, http.StatusServiceUnavailable, false},
	} {
		version, status = tc.body, tc.status
		err := m.nativeReady(context.Background(), n)
		if err == nil || errors.Is(err, ErrServingVersion) != tc.permanent {
			t.Fatalf("%q %d: %v", tc.body, tc.status, err)
		}
	}
	version, status = `{"version":"0.14.1"}`, http.StatusOK
	missingKey := n
	missingKey.EnvironmentFile = filepath.Join(t.TempDir(), "missing.env")
	if err := m.nativeReady(context.Background(), missingKey); err == nil {
		t.Fatal("readiness without the API key file accepted")
	}
	missingLaunch := n
	missingLaunch.LaunchFile = filepath.Join(t.TempDir(), "missing.service")
	if err := m.nativeReady(context.Background(), missingLaunch); err == nil {
		t.Fatal("readiness without the launch file accepted")
	}
	closed := &SystemdManager{client: http.DefaultClient}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := closed.fetchServingVersion(ctx, n); err == nil {
		t.Fatal("canceled version check accepted")
	}
	bad := n
	bad.Endpoint = "://invalid"
	if err := closed.fetchServingVersion(context.Background(), bad); err == nil {
		t.Fatal("invalid endpoint accepted")
	}
}

func TestServingModelDirectory(t *testing.T) {
	owned := control.NativeModel{Owned: &control.OwnedLaunch{ModelPath: "/models/owned"}}
	if dir, err := servingModelDirectory(owned); err != nil || dir != "/models/owned" {
		t.Fatalf("owned = %q, %v", dir, err)
	}
	n := servingBinding(t, "http://127.0.0.1:9000", "selected")
	dir, err := servingModelDirectory(n)
	if err != nil || filepath.Base(dir) != "model" {
		t.Fatalf("adopted = %q, %v", dir, err)
	}
	for name, content := range map[string]string{
		"short command": "[Service]\nExecStart=/usr/bin/vllm serve\n",
		"other runtime": "[Service]\nExecStart=/usr/bin/python3 serve /m\n",
		"bad source":    "[Service]\nExecStart=/usr/bin/vllm serve /m\nEnvironment=PATH=relative\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "unit.service")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := servingModelDirectory(control.NativeModel{Runtime: "vllm", LaunchFile: path}); err == nil {
				t.Fatal("unsupported launch accepted")
			}
		})
	}
}

func TestBindHardenedNativeLaunchReadsAdoptedSettings(t *testing.T) {
	n := servingBinding(t, "http://127.0.0.1:9000", "selected")
	recorded := n
	unbound := n
	unbound.ChatTemplate, unbound.ChatTemplateSHA256, unbound.EnvironmentFile = "", "", "/stale/key.env"
	bound, _, err := BindHardenedNativeLaunch(n.LaunchFile, unbound)
	if bound.ChatTemplate != recorded.ChatTemplate || bound.ChatTemplateSHA256 != recorded.ChatTemplateSHA256 || bound.EnvironmentFile != recorded.EnvironmentFile {
		t.Fatalf("settings not read from the launch: %+v", bound)
	}
	// Executable trust depends on the account that runs the test.
	if err != nil && !errors.Is(err, ErrLaunchUnsupported) {
		t.Fatalf("unexpected qualification error: %v", err)
	}
	if _, _, err := BindHardenedNativeLaunch(filepath.Join(t.TempDir(), "missing.service"), unbound); err == nil {
		t.Fatal("missing launch accepted")
	}
	owned := recorded
	owned.Owned = &control.OwnedLaunch{ModelPath: "/models/a"}
	owned.ChatTemplate = "/models/a/recorded.jinja"
	kept, _, _ := BindHardenedNativeLaunch(n.LaunchFile, owned)
	if kept.ChatTemplate != "/models/a/recorded.jinja" {
		t.Fatal("owned settings replaced")
	}
	for name, content := range map[string]string{
		"unparsable":  "[Service]\nEnvironment=PATH=relative\nExecStart=/usr/bin/vllm serve /m\n",
		"short":       "[Service]\nEnvironmentFile=/k.env\nExecStart=/usr/bin/vllm serve\n",
		"no template": "[Service]\nEnvironmentFile=/k.env\nExecStart=/usr/bin/vllm serve /m --port 1\n",
	} {
		t.Run(name, func(t *testing.T) {
			var binding control.NativeModel
			binding.Runtime = "vllm"
			readHardenedSettings([][]byte{[]byte(content)}, &binding)
			if binding.ChatTemplate != "" || binding.ChatTemplateSHA256 != "" {
				t.Fatalf("template read from %s: %+v", name, binding)
			}
		})
	}
}

// The rendered unit and its digest name only the key file path. The key stays
// in the private file.
func TestOwnedRenderHoldsKeyFilePathNotKey(t *testing.T) {
	h := newHardenedFixture(t)
	p := ownedRenderProfile("vllm", "vision", &control.OwnedLaunch{ModelPath: "/models/v", Port: 9100})
	p.NativeModel = ptrNativeModel(h.bind(*p.NativeModel))
	raw, err := RenderOwnedUnit(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "EnvironmentFile="+h.keyFile+"\n") || strings.Contains(string(raw), strings.Repeat("k", 43)) {
		t.Fatalf("rendered unit:\n%s", raw)
	}
}

func TestQualifyOwnedUnitBeforeKeyAllowsOnlyMissingKey(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "vllm"), []byte("#!/bin/false\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	saved := ownedBinaryDirectory
	ownedBinaryDirectory = dir
	t.Cleanup(func() { ownedBinaryDirectory = saved })
	model := filepath.Join(dir, "model")
	if err := os.Mkdir(model, 0o700); err != nil {
		t.Fatal(err)
	}
	h := newHardenedFixture(t)
	p := ownedRenderProfile("vllm", "vision", &control.OwnedLaunch{ModelPath: model, Port: 9100})
	p.NativeModel = ptrNativeModel(h.bind(*p.NativeModel))
	p.NativeModel.Model = model
	missing := *p.NativeModel
	missing.EnvironmentFile = filepath.Join(t.TempDir(), "created-on-apply.env")
	p.NativeModel = &missing
	if err := QualifyOwnedUnitBeforeKey(p); err != nil {
		t.Fatal(err)
	}
	if err := QualifyOwnedUnit(p); err == nil {
		t.Fatal("full qualification accepted a missing key file")
	}
	if err := os.WriteFile(missing.EnvironmentFile, []byte("unusable"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := QualifyOwnedUnitBeforeKey(p); err == nil {
		t.Fatal("unusable existing key file accepted")
	}
	if err := QualifyOwnedUnitBeforeKey(control.WorkloadProfile{}); err != nil {
		t.Fatal(err)
	}
	broken := p
	broken.NativeModel = ptrNativeModel(*p.NativeModel)
	broken.NativeModel.Owned = &control.OwnedLaunch{ModelPath: model, Port: 9100, Executable: "/bin/sh"}
	if err := QualifyOwnedUnitBeforeKey(broken); err == nil {
		t.Fatal("unsupported executable accepted")
	}
}

func TestAllowedVLLMSettingsQualify(t *testing.T) {
	unit, err := parseExternalLaunchSources([][]byte{[]byte("[Service]\nEnvironment=VLLM_LOGGING_LEVEL=INFO VLLM_NO_USAGE_STATS=1 OTHER_SETTING=1\nExecStart=/usr/bin/true x\n")})
	if err != nil || unit.environmentValues["VLLM_LOGGING_LEVEL"] != "INFO" {
		t.Fatalf("supported vLLM settings rejected: %v", err)
	}
}

func TestAdoptedLaunchQualifiesWithTheSettingsItDeclares(t *testing.T) {
	n := servingBinding(t, "http://127.0.0.1:9000", "selected")
	raw, err := os.ReadFile(n.LaunchFile)
	if err != nil {
		t.Fatal(err)
	}
	unbound := n
	unbound.ChatTemplate, unbound.ChatTemplateSHA256, unbound.EnvironmentFile = "", "", ""
	bound, hash, err := bindHardenedNativeLaunch(n.LaunchFile, unbound, fixtureExecutableValidator)
	if err != nil || hash != sha256Hex(raw) || bound.ChatTemplate != n.ChatTemplate || bound.ChatTemplateSHA256 != n.ChatTemplateSHA256 || bound.EnvironmentFile != n.EnvironmentFile {
		t.Fatalf("adopted launch did not qualify: %v %+v", err, bound)
	}
}

func TestKeyFileNeedsTrustedDirectories(t *testing.T) {
	h := newHardenedFixture(t)
	if err := checkKeyFile(h.keyFile); err != nil {
		t.Fatal(err)
	}
	open := filepath.Join(t.TempDir(), "open")
	if err := os.Mkdir(open, 0o700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(open, "vllm.env")
	raw, _ := os.ReadFile(h.keyFile)
	if err := os.WriteFile(key, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := checkKeyFile(key); err == nil || !strings.Contains(err.Error(), "API key file") {
		t.Fatalf("key file in a world-writable directory accepted: %v", err)
	}
}

func TestReadinessReportsRejectedKeyAndUsesNVFP4Floor(t *testing.T) {
	version := `{"version":"0.26.0"}`
	server := httptest.NewServer(servingAPI(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"selected"}]}`)
	}))
	defer server.Close()
	versioned := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			fmt.Fprint(w, version)
			return
		}
		server.Config.Handler.ServeHTTP(w, r)
	}))
	defer versioned.Close()
	m := &SystemdManager{client: versioned.Client()}
	n := servingBinding(t, versioned.URL, "selected")
	if err := m.nativeReady(context.Background(), n); err != nil {
		t.Fatal("a model without NVFP4 needs only 0.14.1:", err)
	}
	model, err := servingModelDirectory(n)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(model, "config.json"), []byte(`{"quantization_config":{"quant_method":"modelopt_fp4"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.nativeReady(context.Background(), n); !errors.Is(err, ErrServingVersion) || !strings.Contains(err.Error(), "0.27.0") {
		t.Fatalf("NVFP4 model below 0.27.0 accepted: %v", err)
	}
	version = `{"version":"0.27.0"}`
	if err := m.nativeReady(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(n.EnvironmentFile, []byte("VLLM_API_KEY="+strings.Repeat("x", 43)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.nativeReady(context.Background(), n); !errors.Is(err, ErrAPIKeyRejected) {
		t.Fatalf("rejected key reported as %v", err)
	}
}

func TestLoadedEnvironmentFileDriftBlocksTheBinding(t *testing.T) {
	m, r, p, cmd := nativeFixture(t, "vllm", "http://127.0.0.1:9000")
	if err := m.verifyNativeBinding(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	r.outputs[cmd] = []byte(strings.Replace(string(r.outputs[cmd]), "EnvironmentFiles="+p.NativeModel.EnvironmentFile, "EnvironmentFiles=/other/vllm.env", 1))
	if err := m.verifyNativeBinding(context.Background(), p); err == nil || !strings.Contains(err.Error(), "EnvironmentFile") {
		t.Fatalf("loaded EnvironmentFile drift accepted: %v", err)
	}
}

func TestReadinessSendsNoKeyToForeignListener(t *testing.T) {
	var authorizations []string
	server := httptest.NewServer(servingAPI(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"selected"}]}`)
	}))
	defer server.Close()
	recording := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			authorizations = append(authorizations, r.Header.Get("Authorization"))
		}
		server.Config.Handler.ServeHTTP(w, r)
	}))
	defer recording.Close()
	saved := checkKeyListener
	checkKeyListener = func(string) error { return vllmkey.ErrForeignListener }
	t.Cleanup(func() { checkKeyListener = saved })
	m := &SystemdManager{client: recording.Client()}
	if err := m.nativeReady(context.Background(), servingBinding(t, recording.URL, "selected")); !errors.Is(err, vllmkey.ErrForeignListener) {
		t.Fatalf("foreign listener accepted: %v", err)
	}
	if len(authorizations) != 0 {
		t.Fatalf("the key reached a foreign listener: %d requests", len(authorizations))
	}
}

func TestEnvironmentFileRejectsGlobCharacters(t *testing.T) {
	dir := t.TempDir()
	literal := filepath.Join(dir, "vllm-[a].env")
	if err := os.WriteFile(literal, []byte(vllmkey.Name+"="+strings.Repeat("k", 43)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{literal, filepath.Join(dir, "*.env"), filepath.Join(dir, "vllm?.env")} {
		if _, err := parseLaunchUnit([]byte("[Service]\nEnvironmentFile=" + path + "\n")); !errors.Is(err, ErrLaunchUnsupported) {
			t.Fatalf("%s accepted: %v", path, err)
		}
		if _, err := parseExternalLaunchSources([][]byte{[]byte("[Service]\nEnvironmentFile=" + path + "\nExecStart=/usr/bin/true x\n")}); !errors.Is(err, ErrLaunchUnsupported) {
			t.Fatalf("%s accepted in an adopted launch: %v", path, err)
		}
	}
}
