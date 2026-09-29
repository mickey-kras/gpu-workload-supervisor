package runtime

import (
	"context"
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

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := strings.Join(append([]string{name}, args...), " ")
	r.calls = append(r.calls, call)
	return r.outputs[call], r.errs[call]
}

func TestObserveKeepsMediaAvailabilitySeparateFromTextOwnership(t *testing.T) {
	runner := &fakeRunner{outputs: map[string][]byte{
		"systemctl --user show --property=LoadState --property=ActiveState --property=SubState -- text.service":  []byte("LoadState=loaded\nActiveState=active\nSubState=running\n"),
		"systemctl --user show --property=LoadState --property=ActiveState --property=SubState -- media.service": []byte("LoadState=loaded\nActiveState=active\nSubState=running\n"),
	}, errs: map[string]error{}}
	manager, err := newSystemdManager(testConfig(), runner, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.TextActive || !snapshot.MediaReady {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestStartUsesUserSystemdWithoutShell(t *testing.T) {
	runner := &fakeRunner{outputs: map[string][]byte{}, errs: map[string]error{}}
	manager, err := newSystemdManager(testConfig(), runner, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background(), control.WorkloadText); err != nil {
		t.Fatal(err)
	}
	if got := runner.calls[len(runner.calls)-1]; got != "systemctl --user start -- text.service" {
		t.Fatalf("call = %q", got)
	}
}

func TestHealthRequiresSuccessStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	config := testConfig()
	config.TextHealthURL = server.URL
	manager, err := newSystemdManager(config, &fakeRunner{}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Healthy(context.Background(), control.WorkloadText); err == nil {
		t.Fatal("expected health failure")
	}
}

func TestConfigurationRejectsNonLoopbackEndpoint(t *testing.T) {
	config := testConfig()
	config.TextHealthURL = "https://example.com/health"
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
	config.MediaHealthURL = server.URL
	config.MediaReleaseURL = server.URL + "/free"
	manager, err := newSystemdManager(config, &fakeRunner{}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(context.Background(), control.WorkloadMedia); err != nil {
		t.Fatal(err)
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
	config.TextHealthURL = server.URL + "/redirect"
	manager, err := NewSystemdManager(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Healthy(context.Background(), control.WorkloadText); err == nil {
		t.Fatal("expected redirect rejection")
	}
}

func testConfig() SystemdConfig {
	return SystemdConfig{
		TextUnit:        "text.service",
		MediaUnit:       "media.service",
		TextHealthURL:   "http://127.0.0.1:8080/health",
		MediaHealthURL:  "http://127.0.0.1:8188/",
		MediaReleaseURL: "http://127.0.0.1:8188/free",
		HealthTimeout:   time.Second,
		GPUIndex:        0,
		ReleaseMaxMiB:   1024,
	}
}

func TestConfigurationRejectsOptionLikeUnit(t *testing.T) {
	config := testConfig()
	config.TextUnit = "--system.service"
	if _, err := newSystemdManager(config, &fakeRunner{}, http.DefaultClient); err == nil {
		t.Fatal("expected unit validation failure")
	}
}

func TestReleasedUsesNVMLBackedMemoryProbe(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"nvidia-smi --query-gpu=memory.used --format=csv,noheader,nounits -i 0": []byte("900\n"),
		},
		errs: map[string]error{},
	}
	manager, err := newSystemdManager(testConfig(), runner, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Released(context.Background()); err != nil {
		t.Fatal(err)
	}
	runner.outputs["nvidia-smi --query-gpu=memory.used --format=csv,noheader,nounits -i 0"] = []byte("2048\n")
	if err := manager.Released(context.Background()); err == nil {
		t.Fatal("expected unreleased GPU memory")
	}
}
