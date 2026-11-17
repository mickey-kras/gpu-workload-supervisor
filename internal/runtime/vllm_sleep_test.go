package runtime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

const sleepGPU = "GPU-11111111-2222-3333-4444-555555555555"

// The process stays alive during sleep. A stop updates the independent
// systemd observation, rather than treating an HTTP response as release.
type sleepRunner struct {
	*fakeRunner
	unitShow string
}

func (r *sleepRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if slices.Contains(args, "stop") {
		r.mu.Lock()
		r.outputs[r.unitShow] = []byte("LoadState=loaded\nActiveState=inactive\nSubState=dead\nControlGroup=\n")
		r.mu.Unlock()
	}
	return r.fakeRunner.Run(ctx, name, args...)
}

func newSleepTestManager(t *testing.T, endpoint string) (*SystemdManager, *fakeRunner) {
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
		NativeModel: &control.NativeModel{Runtime: "vllm", Instance: "local", Endpoint: endpoint, Model: dir, GPUUUID: sleepGPU, LaunchFile: filepath.Join(dir, "gws-owned-text.service"), ChatTemplate: tpl, ChatTemplateSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("{{ messages }}"))), EnvironmentFile: key,
			Owned: &control.OwnedLaunch{Executable: executable, ModelPath: dir, Port: uint16(port), Serving: control.ServingOptions{Version: 1, SleepMode: true}}}}
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
	runner.outputs["/usr/bin/true --query-gpu=uuid --format=csv,noheader -i "+sleepGPU] = []byte(sleepGPU + "\n")
	runner.outputs["/usr/bin/true --query-gpu=memory.free,memory.total --format=csv,noheader,nounits -i "+sleepGPU] = []byte("32768, 32768\n")
	m := &SystemdManager{config: SystemdConfig{Catalog: &control.Catalog{Version: control.CatalogVersionServing, Profiles: []control.WorkloadProfile{p}}, GPUUUID: sleepGPU, SystemctlPath: "/usr/bin/true", NvidiaSMIPath: "/usr/bin/true", HealthTimeout: time.Second},
		runner: &sleepRunner{fakeRunner: runner, unitShow: show}, client: &http.Client{Timeout: time.Second}, cgroups: cgroupFS{root: t.TempDir(), verify: func(int) error { return nil }}, gpuHost: defaultGPUProcessHost(), nativeExecutableValidator: func(string) error { return nil }}
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

func TestVLLMSleepWakeAndReleasedViaSleep(t *testing.T) {
	var sleeping atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret-test-token-12345678901234567890" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/sleep":
			sleeping.Store(true)
		case "/wake_up":
			sleeping.Store(false)
		case "/is_sleeping":
			fmt.Fprintf(w, `{"is_sleeping":%t}`, sleeping.Load())
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	m, _ := newSleepTestManager(t, srv.URL)
	if err := m.Sleep(context.Background(), "text"); err != nil {
		t.Fatal(err)
	}
	if !sleeping.Load() {
		t.Fatal("sleep was not requested")
	}
	if err := m.ReleasedFor(context.Background(), control.WorkloadIdle); err != nil {
		t.Fatal(err)
	}
	snap, err := m.Observe(context.Background())
	if err != nil || snap.Workloads["text"].Active {
		t.Fatalf("sleep observation: %+v %v", snap, err)
	}
	if err := m.Wake(context.Background(), "text"); err != nil {
		t.Fatal(err)
	}
	if sleeping.Load() {
		t.Fatal("wake was not requested")
	}
}

func TestVLLMSleepFallsBackWhenControlOrGPUProofFails(t *testing.T) {
	for _, scenario := range []string{"endpoint unavailable", "not sleeping", "memory retained", "unknown memory", "GPU query failed", "unknown process"} {
		t.Run(scenario, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if scenario == "endpoint unavailable" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				if r.URL.Path == "/is_sleeping" {
					fmt.Fprintf(w, `{"is_sleeping":%t}`, scenario != "not sleeping")
				}
			}))
			defer srv.Close()
			m, r := newSleepTestManager(t, srv.URL)
			query := "/usr/bin/true --query-compute-apps=pid,process_name,used_memory --format=csv,noheader,nounits -i " + sleepGPU
			if scenario == "memory retained" || scenario == "unknown memory" || scenario == "unknown process" {
				memory := "1024"
				if scenario == "unknown memory" {
					memory = "N/A"
				}
				r.outputs[query] = []byte("123, vllm, " + memory + "\n")
				m.gpuHost.proc = t.TempDir()
				if scenario != "unknown process" {
					(gpuProcFixture{t: t, root: m.gpuHost.proc}).add(123, m.config.Catalog.Profiles[0].Cgroup, 1000)
				}
			}
			if scenario == "GPU query failed" {
				r.errs[query] = errors.New("unavailable")
			}
			if err := m.Sleep(context.Background(), "text"); err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(r.calls, "/usr/bin/true --user stop -- gws-owned-text.service") {
				t.Fatalf("no stop after %s: %v", scenario, r.calls)
			}
		})
	}
}

func TestVLLMWakeRefusesUnobservedStateAndExternalGPUWork(t *testing.T) {
	for _, scenario := range []string{"state unavailable", "external process", "capacity unavailable", "binding drift"} {
		t.Run(scenario, func(t *testing.T) {
			var requests atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
			defer srv.Close()
			m, r := newSleepTestManager(t, srv.URL)
			switch scenario {
			case "state unavailable":
				r.errs["/usr/bin/true --user show --property=LoadState --property=ActiveState --property=SubState --property=ControlGroup -- gws-owned-text.service"] = errors.New("systemd unavailable")
			case "external process":
				r.outputs["/usr/bin/true --query-compute-apps=pid,process_name,used_memory --format=csv,noheader,nounits -i "+sleepGPU] = []byte("123, other, 1024\n")
				m.gpuHost.proc = t.TempDir()
				(gpuProcFixture{t: t, root: m.gpuHost.proc}).add(123, "/other.service", 1000)
			case "capacity unavailable":
				r.outputs["/usr/bin/true --query-gpu=memory.free,memory.total --format=csv,noheader,nounits -i "+sleepGPU] = []byte("100, 32768\n")
			case "binding drift":
				if err := os.WriteFile(m.config.Catalog.Profiles[0].NativeModel.LaunchFile, []byte("changed"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := m.Wake(context.Background(), "text"); err == nil {
				t.Fatal("unsafe wake accepted")
			}
			if requests.Load() != 0 {
				t.Fatal("runtime request before the start checks passed")
			}
			for _, call := range r.calls {
				if strings.Contains(call, " --user start ") || strings.Contains(call, " --user stop ") {
					t.Fatalf("unexpected effect: %s", call)
				}
			}
		})
	}
}

func TestVLLMWakeFallsBackUntilAwakeIsProven(t *testing.T) {
	for _, body := range []string{`{"is_sleeping":true}`, `{}`, `{"is_sleeping":null}`, `not json`} {
		t.Run(body, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/is_sleeping" {
					fmt.Fprint(w, body)
				}
			}))
			defer srv.Close()
			m, r := newSleepTestManager(t, srv.URL)
			if err := m.Wake(context.Background(), "text"); err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(r.calls, "/usr/bin/true --user stop -- gws-owned-text.service") || !slices.Contains(r.calls, "/usr/bin/true --user start -- gws-owned-text.service") {
				t.Fatalf("an unverified wake skipped cold restart: %v", r.calls)
			}
		})
	}
}
