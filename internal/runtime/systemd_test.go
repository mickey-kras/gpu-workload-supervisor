package runtime

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

type fakeRunner struct {
	outputs map[string][]byte
	errs    map[string]error
	calls   []string
}

const rootShowCommand = "/usr/bin/true --user show --property=LoadState --property=ActiveState --property=SubState --property=ControlGroup -- -.slice"
const mediaShowCommand = "/usr/bin/true --user show --property=LoadState --property=ActiveState --property=SubState --property=ControlGroup -- media.service"
const textShowCommand = "/usr/bin/true --user show --property=LoadState --property=ActiveState --property=SubState --property=ControlGroup -- text.service"
const gpuFreeCommand = "/usr/bin/true --query-gpu=memory.free --format=csv,noheader,nounits -i 0"
const gpuMemoryCommand = "/usr/bin/true --query-gpu=memory.used --format=csv,noheader,nounits -i 0"

type recoveryRunner struct{ fakeRunner }

func (r *recoveryRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	command := strings.Join(append([]string{name}, args...), " ")
	if command == "/usr/bin/true --user stop -- text.service" {
		r.outputs[textShowCommand] = []byte("LoadState=loaded\nActiveState=inactive\nSubState=dead\nControlGroup=\n")
	}
	if command == "/usr/bin/true --user stop -- media.service" {
		r.outputs[mediaShowCommand] = []byte("LoadState=loaded\nActiveState=inactive\nSubState=dead\nControlGroup=\n")
	}
	return r.fakeRunner.Run(ctx, name, args...)
}

func TestRecoveryStopsBothSystemdUnitsRatherThanOnlyReleasingMediaModels(t *testing.T) {
	runner := &recoveryRunner{fakeRunner: fakeRunner{outputs: map[string][]byte{
		textShowCommand:  []byte("LoadState=loaded\nActiveState=active\nSubState=running\n"),
		mediaShowCommand: []byte("LoadState=loaded\nActiveState=active\nSubState=running\n"),
		gpuMemoryCommand: []byte("0\n"),
	}}}
	manager, err := newSystemdManager(testConfig(), runner, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	fixtureCgroups(t, manager)
	if err := manager.StopForRecovery(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 4 ||
		runner.calls[0] != "/usr/bin/true --user stop -- text.service" ||
		runner.calls[1] != textShowCommand ||
		runner.calls[2] != "/usr/bin/true --user stop -- media.service" ||
		runner.calls[3] != mediaShowCommand {
		t.Fatalf("recovery stop calls = %#v", runner.calls)
	}
	snapshot, err := manager.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.AnyActive() {
		t.Fatalf("runtime remained active: %#v", snapshot)
	}
	if err := manager.ReleasedFor(context.Background(), control.WorkloadIdle); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryReportsMediaUnitStopFailure(t *testing.T) {
	runner := &fakeRunner{outputs: map[string][]byte{textShowCommand: stoppedOutput()}, errs: map[string]error{
		"/usr/bin/true --user stop -- media.service": errors.New("unit failed to stop"),
	}}
	manager, err := newSystemdManager(testConfig(), runner, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.StopForRecovery(context.Background()); err == nil || !strings.Contains(err.Error(), "command failed") {
		t.Fatalf("media stop failure = %v", err)
	}
}

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := strings.Join(append([]string{name}, args...), " ")
	r.calls = append(r.calls, call)
	if call == rootShowCommand {
		if _, configured := r.outputs[call]; !configured && r.errs[call] == nil {
			return []byte("LoadState=loaded\nActiveState=active\nSubState=active\nControlGroup=/workloads\n"), nil
		}
	}
	return r.outputs[call], r.errs[call]
}

func TestObserveKeepsMediaAvailabilitySeparateFromTextOwnership(t *testing.T) {
	runner := &fakeRunner{outputs: map[string][]byte{
		"/usr/bin/true --user show --property=LoadState --property=ActiveState --property=SubState --property=ControlGroup -- text.service":  []byte("LoadState=loaded\nActiveState=active\nSubState=running\n"),
		"/usr/bin/true --user show --property=LoadState --property=ActiveState --property=SubState --property=ControlGroup -- media.service": []byte("LoadState=loaded\nActiveState=active\nSubState=running\n"),
	}, errs: map[string]error{}}
	manager, err := newSystemdManager(testConfig(), runner, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Workloads[control.WorkloadText].Active || !snapshot.Workloads[control.WorkloadMedia].Active {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestStartUsesUserSystemdWithoutShell(t *testing.T) {
	runner := stoppedRunner()
	manager, err := newSystemdManager(testConfig(), runner, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	fixtureCgroups(t, manager)
	if err := manager.Start(context.Background(), control.WorkloadText); err != nil {
		t.Fatal(err)
	}
	if got := runner.calls[len(runner.calls)-1]; got != "/usr/bin/true --user start -- text.service" {
		t.Fatalf("call = %q", got)
	}
}

func TestHealthRequiresSuccessStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	config := testConfig()
	config.Catalog.Profiles[0].HealthURL = server.URL
	manager, err := newSystemdManager(config, stoppedRunner(), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	fixtureCgroups(t, manager)
	if err := manager.Healthy(context.Background(), control.WorkloadText); err == nil {
		t.Fatal("expected health failure")
	}
}

func TestConfigurationRejectsNonLoopbackEndpoint(t *testing.T) {
	config := testConfig()
	config.Catalog.Profiles[0].HealthURL = "https://example.com/health"
	if _, err := newSystemdManager(config, &fakeRunner{}, http.DefaultClient); err == nil {
		t.Fatal("expected endpoint validation failure")
	}
}

func TestMediaStopUsesReleaseEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/free" {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		response.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	config := testConfig()
	config.Catalog.Profiles[1].HealthURL = server.URL
	config.Catalog.Profiles[1].ReleaseURL = server.URL + "/free"
	runner := &fakeRunner{outputs: map[string][]byte{
		mediaShowCommand: []byte("LoadState=loaded\nActiveState=active\nSubState=running\n"),
	}}
	manager, err := newSystemdManager(config, runner, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(context.Background(), control.WorkloadMedia); err != nil {
		t.Fatal(err)
	}
}

func TestInactiveMediaReleaseIgnoresDesktopMemory(t *testing.T) {
	for _, used := range []string{"286", "287", "999999", "N/A"} {
		runner := stoppedRunner()
		runner.outputs[gpuMemoryCommand] = []byte(used)
		manager, err := newSystemdManager(testConfig(), runner, http.DefaultClient)
		if err != nil {
			t.Fatal(err)
		}
		fixtureCgroups(t, manager)
		if err := manager.Stop(context.Background(), control.WorkloadMedia); err != nil {
			t.Fatal(err)
		}
		if err := manager.ReleasedFor(context.Background(), control.WorkloadIdle); err != nil {
			t.Fatalf("desktop memory %s: %v", used, err)
		}
	}
}

func TestRedirectIsNotAcceptedAsHealthy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/redirect" {
			http.Redirect(response, request, "/healthy", http.StatusFound)
			return
		}
		response.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	config := testConfig()
	config.Catalog.Profiles[0].HealthURL = server.URL + "/redirect"
	manager, err := NewSystemdManager(config)
	if err != nil {
		t.Fatal(err)
	}
	manager.runner = stoppedRunner()
	fixtureCgroups(t, manager)
	if err := manager.Healthy(context.Background(), control.WorkloadText); err == nil {
		t.Fatal("expected redirect rejection")
	}
}

func testConfig() SystemdConfig {
	return SystemdConfig{
		Catalog: &control.Catalog{Version: 1, Profiles: []control.Profile{
			{ID: control.WorkloadText, Label: "text", Adapter: "systemd", Unit: "text.service", Cgroup: "/workloads/text.service", HealthURL: "http://127.0.0.1:8080/health"},
			{ID: control.WorkloadMedia, Label: "media", Adapter: control.AdapterMediaUnload, Unit: "media.service", Cgroup: "/workloads/media.service", HealthURL: "http://127.0.0.1:8188/", ReleaseURL: "http://127.0.0.1:8188/free"},
		}},
		HealthTimeout: time.Second,
		GPUIndex:      0,
		NvidiaSMIPath: "/usr/bin/true",
		SystemctlPath: "/usr/bin/true",
	}
}

func TestConfigurationRejectsOptionLikeUnit(t *testing.T) {
	config := testConfig()
	config.Catalog.Profiles[0].Unit = "--system.service"
	if _, err := newSystemdManager(config, &fakeRunner{}, http.DefaultClient); err == nil {
		t.Fatal("expected unit validation failure")
	}
}

func TestConfigurationRejectsRelativeGPUProbe(t *testing.T) {
	config := testConfig()
	config.NvidiaSMIPath = "nvidia-smi"
	if _, err := newSystemdManager(config, &fakeRunner{}, http.DefaultClient); err == nil {
		t.Fatal("expected GPU probe path validation failure")
	}
}

func TestCatalogCapacityRequirementTriggersGPUProbeValidation(t *testing.T) {
	config := testConfig()
	if config.measuresCapacity() {
		t.Fatal("zero requirements measured capacity")
	}
	c := acceptanceCatalog()
	config.Catalog = &c
	if config.measuresCapacity() {
		t.Fatal("zero profile requirements measured capacity")
	}
	c.Profiles[0].RequiredMiB = 100
	config.NvidiaSMIPath = ""
	if !config.measuresCapacity() {
		t.Fatal("measured profile requirement ignored")
	}
	if _, err := newSystemdManager(config, &fakeRunner{}, http.DefaultClient); err == nil {
		t.Fatal("catalog capacity check accepted missing GPU probe")
	}
}
