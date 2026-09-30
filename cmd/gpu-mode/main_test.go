package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
)

func TestCLIRejectsMissingConfigurationAndUnknownCommand(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"invalid flag", []string{"gpu-mode", "-unknown"}, "flag provided but not defined"},
		{"missing command", []string{"gpu-mode"}, "usage:"},
		{"missing runtime config", []string{"gpu-mode", "status"}, "runtime units"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := os.Args
			os.Args = tc.args
			t.Cleanup(func() { os.Args = previous })
			if err := run(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("run error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestCLIStatusFailsClosedWhenTrustedProbeCannotObserveRuntime(t *testing.T) {
	dir := t.TempDir()
	probe := "/usr/bin/true"
	previousArgs, previousStdout := os.Args, os.Stdout
	t.Cleanup(func() { os.Args, os.Stdout = previousArgs, previousStdout })
	statePath := filepath.Join(dir, "state.db")
	args := []string{"gpu-mode", "-state", statePath, "-text-unit", "text.service",
		"-media-unit", "media.service", "-text-health-url", "http://127.0.0.1:1/health",
		"-media-health-url", "http://127.0.0.1:1/", "-media-release-url", "http://127.0.0.1:1/free",
		"-release-max-used-mib", "1", "-nvidia-smi", probe, "-systemctl", probe}
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = write
	os.Args = append(append([]string{}, args...), "status")
	if err := run(); err == nil || !strings.Contains(err.Error(), "load state") {
		t.Fatalf("observation failure = %v", err)
	}
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(read)
	if err != nil {
		t.Fatal(err)
	}
	if err := read.Close(); err != nil {
		t.Fatal(err)
	}
	var state control.State
	if err := json.Unmarshal(output, &state); err != nil {
		t.Fatalf("status is not JSON: %s: %v", output, err)
	}
	if state.Admission != control.AdmissionClosed || state.Health != control.HealthError {
		t.Fatalf("unsafe status: %#v", state)
	}
	os.Stdout = previousStdout
	os.Args = append(append([]string{}, args...), "unknown")
	if err := run(); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("unknown command error = %v", err)
	}
}

func TestDefaultStatePathUsesXDGDirectory(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	if got, want := defaultStatePath(), filepath.Join(root, "gpu-workload-supervisor", "state.db"); got != want {
		t.Fatalf("default path = %q, want %q", got, want)
	}
	t.Setenv("XDG_STATE_HOME", "")
	if got := defaultStatePath(); !strings.HasSuffix(got, filepath.Join(".local", "state", "gpu-workload-supervisor", "state.db")) {
		t.Fatalf("home default path = %q", got)
	}
}

func TestCLICommandsFailClosedWhenSystemdCannotBeObserved(t *testing.T) {
	previousArgs, previousStdout := os.Args, os.Stdout
	t.Cleanup(func() { os.Args, os.Stdout = previousArgs, previousStdout })
	for _, command := range []string{"reconcile", "recover", "text", "media", "idle", "take-control", "user-switch", "return-control", "recover-user"} {
		t.Run(command, func(t *testing.T) {
			args := []string{"gpu-mode", "-state", filepath.Join(t.TempDir(), "state.db"),
				"-text-unit", "text.service", "-media-unit", "media.service",
				"-text-health-url", "http://127.0.0.1:1/health",
				"-media-health-url", "http://127.0.0.1:1/",
				"-media-release-url", "http://127.0.0.1:1/free",
				"-release-max-used-mib", "1", "-nvidia-smi", "/usr/bin/true",
				"-systemctl", "/usr/bin/true"}
			if command == "take-control" || command == "user-switch" || command == "return-control" || command == "recover-user" {
				args = append(args, "-target", "text")
			}
			args = append(args, command)
			output, err := os.CreateTemp(t.TempDir(), "output")
			if err != nil {
				t.Fatal(err)
			}
			os.Args, os.Stdout = args, output
			if err := run(); err == nil {
				t.Fatal("unobserved runtime accepted")
			}
			if _, err := output.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			var state control.State
			if err := json.NewDecoder(output).Decode(&state); err != nil {
				t.Fatal(err)
			}
			if state.Admission != control.AdmissionClosed {
				t.Fatalf("command opened admission: %#v", state)
			}
			output.Close()
		})
	}
}

func TestCLIRejectsInvalidControllerTimeout(t *testing.T) {
	previousArgs := os.Args
	t.Cleanup(func() { os.Args = previousArgs })
	os.Args = []string{"gpu-mode", "-state", filepath.Join(t.TempDir(), "state.db"),
		"-text-unit", "text.service", "-media-unit", "media.service",
		"-text-health-url", "http://127.0.0.1:1/health",
		"-media-health-url", "http://127.0.0.1:1/",
		"-media-release-url", "http://127.0.0.1:1/free",
		"-release-max-used-mib", "1", "-nvidia-smi", "/usr/bin/true",
		"-systemctl", "/usr/bin/true", "-action-timeout", "0s", "status"}
	if err := run(); err == nil || !strings.Contains(err.Error(), "timeouts") {
		t.Fatalf("invalid controller timeout = %v", err)
	}
}

func TestCLIWorkResolutionRequiresReasonAndStoppedProxies(t *testing.T) {
	previousArgs := os.Args
	previousStdout := os.Stdout
	t.Cleanup(func() { os.Args, os.Stdout = previousArgs, previousStdout })
	statePath := filepath.Join(t.TempDir(), "state.db")
	args := []string{"gpu-mode", "-state", statePath,
		"-text-unit", "text.service", "-media-unit", "media.service",
		"-text-health-url", "http://127.0.0.1:1/health",
		"-media-health-url", "http://127.0.0.1:1/",
		"-media-release-url", "http://127.0.0.1:1/free",
		"-release-max-used-mib", "1", "-nvidia-smi", "/usr/bin/true",
		"-systemctl", "/usr/bin/true"}
	os.Args = append(append([]string{}, args...), "resolve-work")
	if err := run(); err == nil || !strings.Contains(err.Error(), "requires -resolve-reason") {
		t.Fatalf("missing reason error = %v", err)
	}
	for _, tc := range []struct {
		reason string
		want   string
	}{
		{" \t\n ", "requires -resolve-reason"},
		{strings.Repeat("x", 513), "resolution reason must contain 1 to 512 bytes"},
	} {
		os.Args = append(append([]string{}, args...), "-resolve-reason", tc.reason, "resolve-work")
		if err := run(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("reason %q: error = %v, want %q", tc.reason, err, tc.want)
		}
		if _, err := os.Stat(statePath); !os.IsNotExist(err) {
			t.Fatalf("invalid reason opened state store: %v", err)
		}
	}
	proxyLock, err := lock.AcquireShared(statePath + ".proxy.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer proxyLock.Close()
	os.Args = append(append([]string{}, args...), "-resolve-reason", "incident-123", "resolve-work")
	if err := run(); err == nil || !strings.Contains(err.Error(), "stop all workload proxies") {
		t.Fatalf("active proxy error = %v", err)
	}
	if err := proxyLock.Close(); err != nil {
		t.Fatal(err)
	}
	output, err := os.CreateTemp(t.TempDir(), "result")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	os.Stdout = output
	os.Args = append(append([]string{}, args...), "-resolve-reason", "incident-123",
		"-verify-timeout", "5ms", "-poll-interval", "1ms", "resolve-work")
	if err := run(); err == nil || !strings.Contains(err.Error(), "verify release") {
		t.Fatalf("unverified resolution error = %v", err)
	}
	if _, err := output.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	var state control.State
	if err := json.NewDecoder(output).Decode(&state); err != nil {
		t.Fatal(err)
	}
	if state.Admission != control.AdmissionClosed || state.LeaseFence.Epoch != 2 {
		t.Fatalf("failed verification state = %#v", state)
	}
}

func TestOwnershipCLIRequiresExplicitValidTargetBeforeOpeningStore(t *testing.T) {
	previousArgs := os.Args
	t.Cleanup(func() { os.Args = previousArgs })
	for _, command := range []string{"take-control", "user-switch", "return-control", "recover-user"} {
		for _, target := range []string{"", "unknown", "auto"} {
			statePath := filepath.Join(t.TempDir(), "state.db")
			os.Args = []string{"gpu-mode", "-state", statePath, "-target", target, command}
			if err := run(); err == nil || !strings.Contains(err.Error(), "requires -target") {
				t.Fatalf("%s %q: %v", command, target, err)
			}
			if _, err := os.Stat(statePath); !os.IsNotExist(err) {
				t.Fatalf("invalid command opened state: %v", err)
			}
		}
	}
	os.Args = []string{"gpu-mode", "-target", "text", "status"}
	if err := run(); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("unused target accepted: %v", err)
	}
	for _, command := range []string{"take-control", "user-switch", "return-control", "recover-user"} {
		for _, target := range []string{"text", "media", "idle"} {
			if err := validateTarget(command, target); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestCLIRejectsInvalidMediaStopModeBeforeCreatingState(t *testing.T) {
	previous := os.Args
	t.Cleanup(func() { os.Args = previous })
	for _, mode := range []string{"", "STOP-SERVICE", "stop"} {
		path := filepath.Join(t.TempDir(), "state.db")
		os.Args = []string{"gpu-mode", "-state", path, "-media-stop-mode", mode, "status"}
		if err := run(); err == nil || !strings.Contains(err.Error(), "media stop mode") {
			t.Fatalf("mode %q: %v", mode, err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("state created: %v", err)
		}
		if _, err := os.Stat(path + ".lock"); !os.IsNotExist(err) {
			t.Fatalf("lock created: %v", err)
		}
	}
}

func TestCLIStopServiceDoesNotRequireReleaseEndpoint(t *testing.T) {
	previous := os.Args
	t.Cleanup(func() { os.Args = previous })
	os.Args = []string{"gpu-mode", "-state", filepath.Join(t.TempDir(), "state.db"), "-media-stop-mode", "stop-service", "-text-unit", "text.service", "-media-unit", "media.service", "-text-health-url", "http://127.0.0.1:1/health", "-media-health-url", "http://127.0.0.1:1/health", "-release-max-used-mib", "1", "-systemctl", "/usr/bin/true", "-nvidia-smi", "/usr/bin/true", "status"}
	if err := run(); err == nil || !strings.Contains(err.Error(), "load state") {
		t.Fatalf("expected observation failure, got %v", err)
	}
}
