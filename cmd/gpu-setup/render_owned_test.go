package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
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
	input := `{"draft":{"id":"vision","label":"Vision","app":"llama.cpp","binding":{"instance":"owned","owned":{"modelPath":"/models/vision.gguf","port":9100,"ctxSize":8192}}},"managerCgroup":"/user.slice/user-1000.slice/user@1000.service"}`
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
	if result.Profile.ID != "vision" || result.Profile.NativeModel.Runtime != "llama.cpp" || result.Profile.NativeModel.Owned.Port != 9100 {
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
		if err := actions.renderOwned(home, strings.NewReader(invalid), &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted %q", invalid[:min(len(invalid), 40)])
		}
	}
	bad := strings.Replace(input, `"port":9100`, `"port":80`, 1)
	if err := actions.renderOwned(home, strings.NewReader(bad), &bytes.Buffer{}); err == nil {
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
	input := `{"draft":{"id":"vision","label":"Vision","app":"llama.cpp","binding":{"instance":"owned","owned":{"modelPath":"/models/vision.gguf","port":9100}}},"managerCgroup":"/user.slice/user-1000.slice/user@1000.service"}`
	if err := actions.renderOwned(home, strings.NewReader(input), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	foreign := strings.Replace(input, `user@1000.service`, `user@1001.service`, 1)
	if err := actions.renderOwned(home, strings.NewReader(foreign), &bytes.Buffer{}); !errors.Is(err, setup.ErrManagerCgroupMismatch) {
		t.Fatalf("foreign manager cgroup accepted: %v", err)
	}
	empty := strings.Replace(input, `"managerCgroup":"/user.slice/user-1000.slice/user@1000.service"`, `"managerCgroup":""`, 1)
	if err := actions.renderOwned(home, strings.NewReader(empty), &bytes.Buffer{}); !errors.Is(err, setup.ErrManagerCgroupMismatch) {
		t.Fatalf("empty manager cgroup accepted: %v", err)
	}
	actions.managerCgroup = func(context.Context, string) (string, error) { return "", errors.New("no user manager") }
	if err := actions.renderOwned(home, strings.NewReader(input), &bytes.Buffer{}); err == nil {
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
