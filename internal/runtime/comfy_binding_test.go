package runtime

import (
	"context"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestComfyLaunchBindingRecheckedBeforeStarting(t *testing.T) {
	script := comfyScriptFixture(t)
	raw := []byte("[Service]\nType=exec\nExecStart=/usr/bin/python3 " + script + " --port 9000\n")
	path := filepath.Join(t.TempDir(), "comfy.service")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	launch, err := inspectAutomaticLaunch(raw, "comfyui", fixtureExecutableValidator)
	if err != nil {
		t.Fatal(err)
	}
	p := control.WorkloadProfile{ID: "comfy", Adapter: "systemd", Unit: "comfy.service", Cgroup: "/workloads/comfy.service", HealthURL: launch.Endpoint + "/system_stats", LaunchBinding: &control.LaunchBinding{Runtime: "comfyui", Endpoint: launch.Endpoint, LaunchFile: path, LaunchSHA256: launch.SHA256}}
	cmd := "/usr/bin/true --user show --property=FragmentPath --property=DropInPaths --property=NeedDaemonReload -- comfy.service"
	r := &fakeRunner{outputs: map[string][]byte{cmd: []byte("FragmentPath=" + path + "\nDropInPaths=\nNeedDaemonReload=no\n")}}
	m := &SystemdManager{config: SystemdConfig{SystemctlPath: "/usr/bin/true", Catalog: &control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{p}}}, runner: r, nativeExecutableValidator: fixtureExecutableValidator}
	if err := m.verifyNativeBinding(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	saved := *p.LaunchBinding
	for _, field := range []string{"hash", "endpoint", "path", "runtime"} {
		copy := saved
		p.LaunchBinding = &copy
		switch field {
		case "hash":
			copy.LaunchSHA256 = strings.Repeat("0", 64)
		case "endpoint":
			copy.Endpoint = "http://127.0.0.1:1"
		case "path":
			copy.LaunchFile = "/missing"
		case "runtime":
			copy.Runtime = "unsupported"
		}
		if err := m.verifyNativeBinding(context.Background(), p); err == nil {
			t.Fatal(field)
		}
	}
	p.LaunchBinding = &saved
	os.WriteFile(path, []byte("[Service]\nExecStart=/bin/sh -c unsupported\n"), 0600)
	if err := m.verifyNativeBinding(context.Background(), p); err == nil {
		t.Fatal("wrapper drift accepted")
	}
	os.WriteFile(path, raw, 0600)
	m.nativeExecutableValidator = nil
	if err := m.verifyNativeBinding(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	p.SystemdSlice = "app.slice"
	p.SystemdVersion = 255
	r.outputs["/usr/bin/true --version"] = []byte("systemd 999")
	if err := m.verifyNativeBinding(context.Background(), p); err == nil {
		t.Fatal("version drift accepted")
	}
}
