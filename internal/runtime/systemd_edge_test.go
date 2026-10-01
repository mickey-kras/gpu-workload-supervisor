package runtime

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestExecRunnerPassesArgumentsWithoutShell(t *testing.T) {
	output, err := (ExecRunner{}).Run(context.Background(), "/usr/bin/printf", "%s", "one; exit 1")
	if err != nil || string(output) != "one; exit 1" {
		t.Fatalf("output = %q, error = %v", output, err)
	}
	if _, err := (ExecRunner{}).Run(context.Background(), "/usr/bin/false"); err == nil {
		t.Fatal("expected command failure")
	}
}

func TestManagerRejectsUnsafeConfiguration(t *testing.T) {
	tests := []struct {
		name   string
		change func(*SystemdConfig)
	}{
		{"missing unit", func(c *SystemdConfig) { c.TextUnit = "" }},
		{"same unit", func(c *SystemdConfig) { c.MediaUnit = c.TextUnit }},
		{"negative GPU", func(c *SystemdConfig) { c.GPUIndex = -1 }},
		{"missing cgroup", func(c *SystemdConfig) { c.TextCgroup = "" }},
		{"missing systemctl", func(c *SystemdConfig) { c.SystemctlPath = "/not/a/command" }},
		{"zero timeout", func(c *SystemdConfig) { c.HealthTimeout = 0 }},
		{"non-HTTP endpoint", func(c *SystemdConfig) { c.MediaHealthURL = "file:///tmp/health" }},
		{"remote release", func(c *SystemdConfig) { c.MediaReleaseURL = "https://example.com/free" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := testConfig()
			test.change(&config)
			if _, err := newSystemdManager(config, &fakeRunner{}, http.DefaultClient); err == nil {
				t.Fatal("unsafe configuration accepted")
			}
		})
	}
	config := testConfig()
	if _, err := newSystemdManager(config, nil, http.DefaultClient); err == nil {
		t.Fatal("nil runner accepted")
	}
	if _, err := newSystemdManager(config, &fakeRunner{}, nil); err == nil {
		t.Fatal("nil HTTP client accepted")
	}
}

func TestObserveRejectsAmbiguousAndFailedSystemdState(t *testing.T) {
	runner := &fakeRunner{outputs: map[string][]byte{}, errs: map[string]error{}}
	manager, err := newSystemdManager(testConfig(), runner, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	text := "/usr/bin/true --user show --property=LoadState --property=ActiveState --property=SubState --property=ControlGroup -- text.service"
	media := "/usr/bin/true --user show --property=LoadState --property=ActiveState --property=SubState --property=ControlGroup -- media.service"
	runner.errs[text] = errors.New("systemd unavailable")
	if _, err := manager.Observe(context.Background()); err == nil {
		t.Fatal("text observation failure accepted")
	}
	delete(runner.errs, text)
	for _, output := range []string{
		"LoadState=not-found\nActiveState=inactive",
		"LoadState=loaded\nActiveState=active\nSubState=dead",
		"LoadState=loaded\nActiveState=unexpected",
	} {
		runner.outputs[text] = []byte(output)
		if _, err := manager.Observe(context.Background()); err == nil {
			t.Fatalf("invalid state accepted: %q", output)
		}
	}
	runner.outputs[text] = []byte("LoadState=loaded\nActiveState=inactive")
	runner.errs[media] = errors.New("media observation failed")
	if _, err := manager.Observe(context.Background()); err == nil {
		t.Fatal("media observation failure accepted")
	}
	delete(runner.errs, media)
	runner.outputs[media] = []byte("LoadState=loaded\nActiveState=active\nSubState=exited")
	snapshot, err := manager.Observe(context.Background())
	if err != nil || snapshot.TextActive || !snapshot.MediaReady {
		t.Fatalf("snapshot = %#v, error = %v", snapshot, err)
	}
}

func TestRuntimeCommandsAndHealthRejectInvalidWorkloads(t *testing.T) {
	runner := stoppedRunner()
	runner.errs = map[string]error{}
	manager, err := newSystemdManager(testConfig(), runner, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	fixtureCgroups(t, manager)
	if err := manager.Start(context.Background(), control.WorkloadMedia); err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(context.Background(), control.WorkloadText); err != nil {
		t.Fatal(err)
	}
	for _, workload := range []control.Workload{control.WorkloadIdle, "invalid"} {
		if err := manager.Start(context.Background(), workload); err == nil {
			t.Fatalf("started %q", workload)
		}
		if err := manager.Stop(context.Background(), workload); err == nil {
			t.Fatalf("stopped %q", workload)
		}
	}
	if err := manager.Healthy(context.Background(), control.WorkloadIdle); err != nil {
		t.Fatal(err)
	}
	if err := manager.Healthy(context.Background(), "invalid"); err == nil {
		t.Fatal("unknown workload was healthy")
	}
	runner.errs["/usr/bin/true --user start -- media.service"] = errors.New("start failed")
	if err := manager.Start(context.Background(), control.WorkloadMedia); err == nil {
		t.Fatal("systemd start failure was ignored")
	}
}

type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, errors.New("body read failed") }
func (failingBody) Close() error             { return nil }

type responseTransport struct {
	response *http.Response
	err      error
}

func (r responseTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return r.response, r.err
}

func TestHealthAndReleaseFailClosedOnTransportAndBodyErrors(t *testing.T) {
	runner := stoppedRunner()
	runner.outputs[mediaShowCommand] = []byte("LoadState=loaded\nActiveState=active\nSubState=running\n")
	manager, err := newSystemdManager(testConfig(), runner, &http.Client{
		Timeout:   time.Second,
		Transport: responseTransport{err: errors.New("offline")},
	})
	if err != nil {
		t.Fatal(err)
	}
	fixtureCgroups(t, manager)
	// Test the HTTP health transport independently after the opposing unit stops.
	liveMedia := runner.outputs[mediaShowCommand]
	runner.outputs[mediaShowCommand] = stoppedOutput()
	if err := manager.Healthy(context.Background(), control.WorkloadText); err == nil {
		t.Fatal("network failure was healthy")
	}
	runner.outputs[mediaShowCommand] = liveMedia
	if err := manager.Stop(context.Background(), control.WorkloadMedia); err == nil {
		t.Fatal("release network failure accepted")
	}
	manager.client.Transport = responseTransport{response: &http.Response{
		StatusCode: http.StatusOK, Body: failingBody{}, Header: make(http.Header),
	}}
	if err := manager.Healthy(context.Background(), control.WorkloadMedia); err == nil {
		t.Fatal("health body error accepted")
	}
	if err := manager.Stop(context.Background(), control.WorkloadMedia); err == nil {
		t.Fatal("release body error accepted")
	}
	manager.client.Transport = responseTransport{response: &http.Response{
		StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader("unavailable")),
		Header: make(http.Header),
	}}
	if err := manager.Stop(context.Background(), control.WorkloadMedia); err == nil {
		t.Fatal("release status failure accepted")
	}
}

func TestMediaStopRejectsSystemdInspectionFailure(t *testing.T) {
	runner := &fakeRunner{errs: map[string]error{
		mediaShowCommand: errors.New("systemd unavailable"),
	}}
	manager, err := newSystemdManager(testConfig(), runner, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(context.Background(), control.WorkloadMedia); err == nil || !strings.Contains(err.Error(), "systemd unavailable") {
		t.Fatalf("expected systemd failure, got %v", err)
	}
}

func TestMediaStopRequiresReleaseEndpointUnlessUnitIsInactive(t *testing.T) {
	for _, state := range []string{"failed", "deactivating", "activating"} {
		t.Run(state, func(t *testing.T) {
			runner := &fakeRunner{outputs: map[string][]byte{
				mediaShowCommand: []byte("LoadState=loaded\nActiveState=" + state + "\n"),
			}}
			releaseErr := errors.New("release unavailable")
			manager, err := newSystemdManager(testConfig(), runner, &http.Client{
				Transport: responseTransport{err: releaseErr},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := manager.Stop(context.Background(), control.WorkloadMedia); err == nil || (!strings.Contains(err.Error(), "media release request") || !errors.Is(err, releaseErr)) {
				t.Fatalf("expected release failure for %s unit, got %v", state, err)
			}
		})
	}
}

func TestExecutablePathRequiresTrustedOwnershipAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "command")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := validateExecutable(path); err == nil {
		t.Fatal("non-executable accepted")
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := validateExecutable(path); err == nil {
		t.Fatal("executable under untrusted directory accepted")
	}
	if _, err := validateExecutable("/missing/command"); err == nil {
		t.Fatal("missing executable accepted")
	}
}
