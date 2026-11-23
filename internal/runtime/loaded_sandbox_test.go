package runtime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

type sandboxDriftRunner struct {
	CommandRunner
	started bool
}

func (r *sandboxDriftRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := r.CommandRunner.Run(ctx, name, args...)
	if slices.Contains(args, "start") {
		r.started = true
	}
	if r.started && slices.Contains(args, "--property=ProtectSystem") {
		return []byte(strings.Replace(string(out), "ProtectSystem=strict", "ProtectSystem=no", 1)), err
	}
	return out, err
}

func TestOwnedSandboxReadback(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	m, r := newOwnedFixture(t, "http://127.0.0.1:9111", true)
	p := m.config.Catalog.Profiles[0]
	ctx := context.Background()
	if err := m.verifyNativeBinding(ctx, p); err != nil {
		t.Fatal(err)
	}
	command := r.calls[len(r.calls)-1]
	good := string(r.outputs[command])
	// systemctl show keeps PrivateUsers boolean even on releases that also
	// expose PrivateUsersEx, prints mode in octal and expands %t at load.
	const writable = "/run/user/1000/gws-d253cbad16732b9d520413ee"
	for _, line := range []string{
		"Type=exec", "PrivateUsers=yes", "ProtectSystem=strict", "MountFlags=private", "RuntimeDirectoryMode=0700",
		"ReadOnlyPaths=" + p.NativeModel.Owned.ModelPath, "ReadWritePaths=" + writable,
		"BindReadOnlyPaths=" + p.NativeModel.Owned.ModelPath + ":" + writable + "/model:rbind",
		"RuntimeDirectory=gws-d253cbad16732b9d520413ee",
	} {
		if !slices.Contains(strings.Split(good, "\n"), line) {
			t.Fatalf("readback expectation differs from systemctl show for %s", line)
		}
	}
	properties, err := ParseUnitProperties(r.outputs[command])
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"XDG_CACHE_HOME", "XDG_CONFIG_HOME", "TMPDIR", "FLASHINFER_WORKSPACE_BASE", "HF_HOME", "VLLM_CACHE_ROOT", "CUDA_CACHE_PATH", "TRITON_CACHE_DIR"} {
		if !slices.Contains(strings.Fields(properties["Environment"]), name+"="+writable) {
			t.Fatalf("missing expanded %s in loaded environment", name)
		}
	}
	for _, key := range []string{"Type", "ProtectSystem", "PrivateUsers", "MountFlags", "ReadOnlyPaths", "ReadWritePaths", "BindReadOnlyPaths", "RuntimeDirectory", "RuntimeDirectoryMode"} {
		t.Run(key, func(t *testing.T) {
			if !slices.Contains(strings.Fields(command), "--property="+key) {
				t.Fatalf("missing %s in systemctl show", key)
			}
			for _, changed := range []string{"Missing=", key + "=unexpected "} {
				r.outputs[command] = []byte(strings.Replace(good, "\n"+key+"=", "\n"+changed, 1))
				if err := m.Healthy(ctx, p.ID); !errors.Is(err, ErrLaunchChanged) || !strings.Contains(err.Error(), key) {
					t.Fatalf("health accepted missing or changed %s: %v", key, err)
				}
			}
			r.outputs[command] = []byte(good)
		})
	}

	m.runner = &sandboxDriftRunner{CommandRunner: m.runner}
	err = m.Start(ctx, p.ID)
	if !errors.Is(err, ErrLaunchChanged) || !strings.Contains(err.Error(), "ProtectSystem") {
		t.Fatalf("post-start drift returned %v", err)
	}
	if !slices.Contains(r.calls, "/usr/bin/true --user stop -- "+p.Unit) {
		t.Fatal("failed sandbox readback left the started unit running")
	}
}

func TestOwnedSandboxLegacyBindingCanStop(t *testing.T) {
	m, r := newOwnedFixture(t, "http://127.0.0.1:9111", false)
	p := m.config.Catalog.Profiles[0]
	ctx := context.Background()
	if err := m.Preflight(ctx); !errors.Is(err, ErrLaunchUnsupported) {
		t.Fatalf("serving preflight accepted unsandboxed launch: %v", err)
	}
	if err := m.PreflightForStop(ctx); err != nil {
		t.Fatalf("verified shutdown preflight = %v", err)
	}
	if err := m.ReleasedFor(ctx, control.WorkloadIdle); err == nil {
		t.Fatal("running legacy unit passed Idle release")
	}
	if err := m.Start(context.Background(), p.ID); !errors.Is(err, ErrLaunchUnsupported) || !strings.Contains(err.Error(), "run setup") {
		t.Fatalf("legacy binding start = %v", err)
	}
	if slices.Contains(r.calls, "/usr/bin/true --user start -- "+p.Unit) {
		t.Fatal("legacy unit started")
	}
	writeEvents(t, m.cgroups.root, strings.TrimPrefix(p.Cgroup, "/"), "populated 0\n")
	if err := m.Stop(ctx, p.ID); err != nil {
		t.Fatalf("legacy binding stop = %v", err)
	}
	if err := m.Healthy(ctx, control.WorkloadIdle); err != nil {
		t.Fatalf("release after verified cold stop = %v", err)
	}
	// Recovery that retains another workload proves this stopped profile released.
	if err := m.checkReleasedProfile(ctx, p, control.Workload("other"), nil); err != nil {
		t.Fatalf("release for another target after verified cold stop = %v", err)
	}
	if err := m.checkReleasedProfile(ctx, p, p.ID, nil); !errors.Is(err, ErrLaunchUnsupported) {
		t.Fatalf("legacy binding passed as release target: %v", err)
	}
	p.NativeModel.LaunchSHA256 = strings.Repeat("f", 64)
	if err := m.PreflightForStop(ctx); !errors.Is(err, ErrOwnedRender) {
		t.Fatalf("shutdown preflight accepted changed fingerprint: %v", err)
	}
}

const ownedGPU = "GPU-11111111-2222-3333-4444-555555555555"

// A stop updates the independent systemd observation.
type stoppingRunner struct {
	*fakeRunner
	unitShow string
}

func (r *stoppingRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if slices.Contains(args, "stop") {
		r.mu.Lock()
		r.outputs[r.unitShow] = []byte("LoadState=loaded\nActiveState=inactive\nSubState=dead\nControlGroup=\n")
		r.mu.Unlock()
	}
	return r.fakeRunner.Run(ctx, name, args...)
}

func newOwnedFixture(t *testing.T, endpoint string, sandbox bool) (*SystemdManager, *fakeRunner) {
	t.Helper()
	dir := t.TempDir()
	key := filepath.Join(dir, "vllm.env")
	tpl := filepath.Join(dir, "chat_template.jinja")
	executable := filepath.Join(dir, "vllm")
	for path, content := range map[string]string{key: "VLLM_API_KEY=secret-test-token-12345678901234567890\n", tpl: "{{ messages }}", executable: "#!/bin/false\n"} {
		if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(key, 0o600); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.ParseUint(u.Port(), 10, 16)
	if err != nil {
		t.Fatal(err)
	}
	p := control.WorkloadProfile{ID: "text", Label: "Text", Adapter: "systemd", Unit: "gws-owned-text.service", Cgroup: "/user.test/app.slice/gws-owned-text.service", HealthURL: endpoint + "/health",
		NativeModel: &control.NativeModel{Runtime: "vllm", Instance: "local", Endpoint: endpoint, Model: dir, GPUUUID: ownedGPU, LaunchFile: filepath.Join(dir, "gws-owned-text.service"), ChatTemplate: tpl, ChatTemplateSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("{{ messages }}"))), EnvironmentFile: key,
			Owned: &control.OwnedLaunch{ArtifactSandbox: sandbox, Executable: executable, ModelPath: dir, Port: uint16(port), Serving: control.ServingOptions{Version: 1}}}}
	raw, err := RenderOwnedUnit(p)
	if err != nil {
		t.Fatal(err)
	}
	p.NativeModel.LaunchSHA256 = fmt.Sprintf("%x", sha256.Sum256(raw))
	if err := os.WriteFile(p.NativeModel.LaunchFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{outputs: map[string][]byte{}, errs: map[string]error{}}
	show := "/usr/bin/true --user show --property=LoadState --property=ActiveState --property=SubState --property=ControlGroup -- " + p.Unit
	runner.outputs[show] = []byte("LoadState=loaded\nActiveState=active\nSubState=running\nControlGroup=" + p.Cgroup + "\n")
	runner.outputs[rootShowCommand] = []byte("LoadState=loaded\nActiveState=active\nSubState=active\nControlGroup=/user.test\n")
	runner.outputs["/usr/bin/true --query-gpu=uuid --format=csv,noheader -i "+ownedGPU] = []byte(ownedGPU + "\n")
	runner.outputs["/usr/bin/true --query-gpu=memory.free,memory.total --format=csv,noheader,nounits -i "+ownedGPU] = []byte("32768, 32768\n")
	catalog := control.Catalog{Profiles: []control.WorkloadProfile{p}}
	catalog.Version = control.LowestCatalogVersion(catalog)
	m := &SystemdManager{config: SystemdConfig{OwnedUnitDir: dir, Catalog: &catalog, GPUUUID: ownedGPU, SystemctlPath: "/usr/bin/true", NvidiaSMIPath: "/usr/bin/true", HealthTimeout: time.Second},
		runner: &stoppingRunner{fakeRunner: runner, unitShow: show}, client: &http.Client{Timeout: time.Second}, cgroups: cgroupFS{root: t.TempDir(), verify: func(int) error { return nil }}, gpuHost: defaultGPUProcessHost(), nativeExecutableValidator: func(string) error { return nil }}
	launch, err := readVerifiedNativeLaunch(*p.NativeModel, m.nativeExecutableValidator)
	if err != nil {
		t.Fatal(err)
	}
	props := "FragmentPath=" + p.NativeModel.LaunchFile + "\nDropInPaths=\nNeedDaemonReload=no\nExecStartPre=\nExecStart={ path=" + executable + " ; argv[]=" + launch.execStart + " ; ignore_errors=no ; }\nEnvironmentFiles=" + key + " (ignore_errors=no)\n"
	var environment []string
	for k, v := range launch.environmentValues {
		environment = append(environment, k+"="+v)
	}
	props += "Environment=" + strings.Join(environment, " ") + "\n"
	for k, v := range launch.loadedProperties {
		props += k + "=" + v + "\n"
	}
	bindingCall := strings.Join(append([]string{"/usr/bin/true"}, nativeBindingShowArguments(p.Unit, launch)...), " ")
	runner.outputs[bindingCall] = []byte(props)
	writeEvents(t, m.cgroups.root, "user.test", "populated 1\n")
	writeEvents(t, m.cgroups.root, strings.TrimPrefix(p.Cgroup, "/"), "populated 1\n")
	old := checkKeyListener
	checkKeyListener = func(string) error { return nil }
	t.Cleanup(func() { checkKeyListener = old })
	return m, runner
}
