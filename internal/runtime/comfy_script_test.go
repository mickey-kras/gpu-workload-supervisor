package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"golang.org/x/sys/unix"
)

func TestComfyScriptTrustRejectedDuringDiscoveryAndBeforeStart(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{"missing", func(t *testing.T, p string) {
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
		}},
		{"world-writable", func(t *testing.T, p string) {
			if err := os.Chmod(p, 0666); err != nil {
				t.Fatal(err)
			}
		}},
		{"group-writable", func(t *testing.T, p string) {
			if err := os.Chmod(p, 0620); err != nil {
				t.Fatal(err)
			}
		}},
		{"unreadable", func(t *testing.T, p string) {
			if err := os.Chmod(p, 0000); err != nil {
				t.Fatal(err)
			}
		}},
		{"directory", func(t *testing.T, p string) {
			os.Remove(p)
			if err := os.Mkdir(p, 0700); err != nil {
				t.Fatal(err)
			}
		}},
		{"fifo", func(t *testing.T, p string) {
			os.Remove(p)
			if err := unix.Mkfifo(p, 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"script-symlink", func(t *testing.T, p string) {
			target := p + ".saved"
			if err := os.Rename(p, target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, p); err != nil {
				t.Fatal(err)
			}
		}},
		{"parent-symlink", func(t *testing.T, p string) {
			dir := filepath.Dir(p)
			if err := os.Rename(dir, dir+".saved"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(dir+".saved", dir); err != nil {
				t.Fatal(err)
			}
		}},
		{"parent-world-writable", func(t *testing.T, p string) {
			if err := os.Chmod(filepath.Dir(p), 0777); err != nil {
				t.Fatal(err)
			}
		}},
		{"ancestor-group-writable", func(t *testing.T, p string) {
			if err := os.Chmod(filepath.Dir(filepath.Dir(p)), 0770); err != nil {
				t.Fatal(err)
			}
		}},
		{"sticky-parent", func(t *testing.T, p string) {
			if err := os.Chmod(filepath.Dir(p), 0777|os.ModeSticky); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			script := comfyScriptFixture(t)
			raw := []byte("[Service]\nType=exec\nExecStart=/usr/bin/python3 " + script + " --port 9000\n")
			unit := filepath.Join(filepath.Dir(filepath.Dir(script)), "comfy.service")
			if err := os.WriteFile(unit, raw, 0600); err != nil {
				t.Fatal(err)
			}
			launch, err := InspectAutomaticLaunch(unit, "comfyui")
			if err != nil {
				t.Fatal("trusted existing script rejected:", err)
			}
			p := control.WorkloadProfile{ID: "comfy", Unit: "comfy.service", LaunchBinding: &control.LaunchBinding{Runtime: "comfyui", Endpoint: launch.Endpoint, LaunchFile: unit, LaunchSHA256: launch.SHA256}}
			cmd := "/usr/bin/true --user show --property=FragmentPath --property=DropInPaths --property=NeedDaemonReload -- comfy.service"
			runner := &fakeRunner{outputs: map[string][]byte{cmd: []byte("FragmentPath=" + unit + "\nDropInPaths=\nNeedDaemonReload=no\n")}}
			manager := &SystemdManager{config: SystemdConfig{SystemctlPath: "/usr/bin/true", Catalog: &control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{p}}}, runner: runner}
			if err := manager.verifyNativeBinding(context.Background(), p); err != nil {
				t.Fatal(err)
			}
			tt.mutate(t, script)
			if _, err := InspectAutomaticLaunch(unit, "comfyui"); err == nil {
				t.Fatal("untrusted application discovered")
			}
			if err := manager.verifyNativeBinding(context.Background(), p); err == nil {
				t.Fatal("binding ignored changed script trust")
			}
			if err := manager.Start(context.Background(), p.ID); err == nil {
				t.Fatal("started untrusted application")
			}
			for _, call := range runner.calls {
				if strings.Contains(call, " --user start ") {
					t.Fatal("unsafe launch reached service start", call)
				}
			}
		})
	}
}

func TestComfyScriptRejectsMalformedPathsAndPreservesTrustedInterpreterAlias(t *testing.T) {
	script := comfyScriptFixture(t)
	for _, path := range []string{"relative/main.py", filepath.Dir(script) + "/../ComfyUI/main.py", script + "\x00"} {
		if err := validateComfyScript(path); err == nil {
			t.Fatal(path)
		}
	}
	// The application need not be executable or private; ordinary readable code
	// owned by the desktop principal is supported, with no other writers.
	if err := os.Chmod(script, 0644); err != nil {
		t.Fatal(err)
	}
	if err := validateComfyScript(script); err != nil {
		t.Fatal(err)
	}
	if err := validateComfyExecutable("/usr/bin/python3"); err != nil {
		t.Fatal("trusted python3 interpreter alias rejected", err)
	}
}
