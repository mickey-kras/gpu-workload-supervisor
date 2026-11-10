package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/setup"
)

func TestRenderOwnedGoldenOutput(t *testing.T) {
	actions := systemActions()
	home := t.TempDir()
	actions.home = func() (string, error) { return home, nil }
	actions.euid = func() int { return 1000 }
	actions.managerCgroup = func(context.Context, string) (string, error) {
		return "/user.slice/user-1000.slice/user@1000.service", nil
	}
	input := `{"draft":{"id":"vision","label":"Vision","app":"vllm","binding":{"instance":"owned","owned":{"modelPath":"` + renderModelPath(t) + `","port":9100,"maxModelLen":8192}}},"managerCgroup":"/user.slice/user-1000.slice/user@1000.service"}`
	var output bytes.Buffer
	if err := actions.run([]string{"render-owned"}, strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Unit       string `json:"unit"`
		Cgroup     string `json:"cgroup"`
		LaunchFile string `json:"launchFile"`
		SHA256     string `json:"sha256"`
		Profile    struct {
			ID          string `json:"id"`
			NativeModel struct {
				Runtime      string `json:"runtime"`
				LaunchSHA256 string `json:"launchSHA256"`
				Owned        struct {
					ModelPath string `json:"modelPath"`
					Port      uint16 `json:"port"`
				} `json:"owned"`
			} `json:"nativeModel"`
		} `json:"profile"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Unit != "gws-owned-vision.service" {
		t.Fatalf("unit %q", result.Unit)
	}
	if result.Cgroup != "/user.slice/user-1000.slice/user@1000.service/app.slice/gws-owned-vision.service" {
		t.Fatalf("cgroup %q", result.Cgroup)
	}
	if result.LaunchFile != home+"/.config/systemd/user/gws-owned-vision.service" || len(result.SHA256) != 64 {
		t.Fatalf("launch file %q sha %q", result.LaunchFile, result.SHA256)
	}
	if result.Profile.ID != "vision" || result.Profile.NativeModel.Runtime != "vllm" || result.Profile.NativeModel.Owned.Port != 9100 {
		t.Fatalf("profile %+v", result.Profile)
	}
	if result.SHA256 != result.Profile.NativeModel.LaunchSHA256 {
		t.Fatal("sha256 disagrees with profile fingerprint")
	}
	// render-owned writes nothing.
	if got := dirEntries(t, home); len(got) != 0 {
		t.Fatalf("render-owned wrote files: %v", got)
	}
	for _, invalid := range []string{`{`, `{}`, input + ` {}`, `{"extra":1}`} {
		if err := actions.renderOwned(context.Background(), home, strings.NewReader(invalid), &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted %q", invalid[:min(len(invalid), 40)])
		}
	}
	bad := strings.Replace(input, `"port":9100`, `"port":80`, 1)
	if err := actions.renderOwned(context.Background(), home, strings.NewReader(bad), &bytes.Buffer{}); err == nil {
		t.Fatal("privileged port accepted")
	}
}

// TestRenderOwnedVerifiesManagerCgroupAgainstHost rejects caller-supplied
// manager cgroups that do not match the running user manager, so a typo or a
// stale value cannot derive an uncontrollable cgroup.
func TestRenderOwnedVerifiesManagerCgroupAgainstHost(t *testing.T) {
	actions := systemActions()
	home := t.TempDir()
	actions.home = func() (string, error) { return home, nil }
	actions.euid = func() int { return 1000 }
	actions.managerCgroup = func(context.Context, string) (string, error) {
		return "/user.slice/user-1000.slice/user@1000.service", nil
	}
	input := `{"draft":{"id":"vision","label":"Vision","app":"vllm","binding":{"instance":"owned","owned":{"modelPath":"` + renderModelPath(t) + `","port":9100}}},"managerCgroup":"/user.slice/user-1000.slice/user@1000.service"}`
	if err := actions.renderOwned(context.Background(), home, strings.NewReader(input), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	foreign := strings.Replace(input, `user@1000.service`, `user@1001.service`, 1)
	if err := actions.renderOwned(context.Background(), home, strings.NewReader(foreign), &bytes.Buffer{}); !errors.Is(err, setup.ErrManagerCgroupMismatch) {
		t.Fatalf("foreign manager cgroup accepted: %v", err)
	}
	empty := strings.Replace(input, `"managerCgroup":"/user.slice/user-1000.slice/user@1000.service"`, `"managerCgroup":""`, 1)
	var derived bytes.Buffer
	if err := actions.renderOwned(context.Background(), home, strings.NewReader(empty), &derived); err != nil {
		t.Fatalf("host-derived manager cgroup rejected: %v", err)
	}
	if !strings.Contains(derived.String(), "/user.slice/user-1000.slice/user@1000.service/app.slice/") {
		t.Fatalf("preview did not derive the observed manager root: %s", derived.String())
	}
	actions.managerCgroup = func(context.Context, string) (string, error) { return "", nil }
	if err := actions.renderOwned(context.Background(), home, strings.NewReader(empty), &bytes.Buffer{}); !errors.Is(err, setup.ErrManagerCgroupMismatch) {
		t.Fatalf("empty observed manager root accepted: %v", err)
	}
	actions.managerCgroup = func(context.Context, string) (string, error) { return "", errors.New("no user manager") }
	if err := actions.renderOwned(context.Background(), home, strings.NewReader(input), &bytes.Buffer{}); err == nil {
		t.Fatal("unavailable manager accepted")
	}
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	var found []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		found = append(found, e.Name())
	}
	return found
}

// TestRenderOwnedHonorsCallerContext blocks the manager-cgroup probe until the
// caller's context ends: render-owned must return instead of hanging forever.
func TestRenderOwnedHonorsCallerContext(t *testing.T) {
	actions := systemActions()
	actions.managerCgroup = func(ctx context.Context, _ string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := actions.renderOwned(ctx, t.TempDir(), strings.NewReader(`{"draft":{}}`), &bytes.Buffer{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("render-owned ignored caller context: %v", err)
	}
}

// renderModelPath returns a model folder with the chat template that an owned
// vLLM launch pins.
func renderModelPath(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "vision")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "chat_template.jinja"), []byte("{{ messages }}"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A draft with limits renders and fingerprints the final unit, and the
// setup decoder accepts the request with its catalog v5 profile. Limits
// merged into a profile after a render without them diverge from the
// recorded fingerprint, so the decoder refuses such a request.
func TestRenderOwnedWithLimitsPassesTheSetupDecoder(t *testing.T) {
	actions := systemActions()
	home := t.TempDir()
	actions.home = func() (string, error) { return home, nil }
	actions.euid = func() int { return 1000 }
	actions.managerCgroup = func(context.Context, string) (string, error) {
		return "/user.slice/user-1000.slice/user@1000.service", nil
	}
	limits := `{"version":1,"memoryHighMiB":8192,"memoryMaxMiB":10240,"tasksMax":4096,"cpuQuotaPercent":400}`
	render := func(owned string) json.RawMessage {
		t.Helper()
		input := `{"draft":{"id":"vision","label":"Vision","app":"vllm","binding":{"instance":"owned","owned":{"modelPath":"` + renderModelPath(t) + `","port":9100` + owned + `}}}}`
		var output bytes.Buffer
		if err := actions.run([]string{"render-owned"}, strings.NewReader(input), &output); err != nil {
			t.Fatal(err)
		}
		var result struct {
			Profile json.RawMessage `json:"profile"`
		}
		if err := json.Unmarshal(output.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result.Profile
	}
	request := func(version int, profile json.RawMessage) string {
		return `{"version":1,"profile":{"version":1,"statePath":"` + home + `/state.db","systemctlPath":"/usr/bin/systemctl","nvidiaSMIPath":"/usr/bin/nvidia-smi"},"catalog":{"version":` + strconv.Itoa(version) +
			`,"profiles":[` + string(profile) + `]},"expectedRevision":"","confirmQuiesced":false}`
	}
	limited := render(`,"limits":` + limits)
	if !strings.Contains(string(limited), `"limits":`+limits) {
		t.Fatalf("rendered profile without the limits: %s", limited)
	}
	if _, err := setup.Decode(strings.NewReader(request(5, limited))); err != nil {
		t.Fatalf("decode of a request rendered with limits = %v", err)
	}
	plain := render("")
	merged := json.RawMessage(strings.Replace(string(plain), `"port":9100`, `"port":9100,"limits":`+limits, 1))
	if _, err := setup.Decode(strings.NewReader(request(5, merged))); err == nil || !strings.Contains(err.Error(), "diverges from the recorded fingerprint") {
		t.Fatalf("decode of limits merged after the render = %v", err)
	}
	if _, err := setup.Decode(strings.NewReader(request(2, plain))); err != nil {
		t.Fatalf("decode of a request without limits = %v", err)
	}
}
