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
		{"invalid flag", []string{"gpu-mode", "-media-stop-mode", "unload", "-unknown"}, "flag provided but not defined"},
		{"missing command", []string{"gpu-mode"}, "usage:"},
		{"missing runtime config", []string{"gpu-mode", "-media-stop-mode", "unload", "status"}, "runtime units"},
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
	args := []string{"gpu-mode", "-media-stop-mode", "unload", "-state", statePath, "-text-unit", "text.service",
		"-media-unit", "media.service", "-text-health-url", "http://127.0.0.1:1/health",
		"-media-health-url", "http://127.0.0.1:1/", "-media-release-url", "http://127.0.0.1:1/free",
		"-text-cgroup", "/text.service", "-media-cgroup", "/media.service", "-nvidia-smi", probe, "-systemctl", probe}
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
			args := []string{"gpu-mode", "-media-stop-mode", "unload", "-state", filepath.Join(t.TempDir(), "state.db"),
				"-text-unit", "text.service", "-media-unit", "media.service",
				"-text-health-url", "http://127.0.0.1:1/health",
				"-media-health-url", "http://127.0.0.1:1/",
				"-media-release-url", "http://127.0.0.1:1/free",
				"-text-cgroup", "/text.service", "-media-cgroup", "/media.service", "-nvidia-smi", "/usr/bin/true",
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
	os.Args = []string{"gpu-mode", "-media-stop-mode", "unload", "-state", filepath.Join(t.TempDir(), "state.db"),
		"-text-unit", "text.service", "-media-unit", "media.service",
		"-text-health-url", "http://127.0.0.1:1/health",
		"-media-health-url", "http://127.0.0.1:1/",
		"-media-release-url", "http://127.0.0.1:1/free",
		"-text-cgroup", "/text.service", "-media-cgroup", "/media.service", "-nvidia-smi", "/usr/bin/true",
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
	args := []string{"gpu-mode", "-media-stop-mode", "unload", "-state", statePath,
		"-text-unit", "text.service", "-media-unit", "media.service",
		"-text-health-url", "http://127.0.0.1:1/health",
		"-media-health-url", "http://127.0.0.1:1/",
		"-media-release-url", "http://127.0.0.1:1/free",
		"-text-cgroup", "/text.service", "-media-cgroup", "/media.service", "-nvidia-smi", "/usr/bin/true",
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
	if err := run(); err == nil || !strings.Contains(err.Error(), "manager cgroup anchor") {
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
			os.Args = []string{"gpu-mode", "-media-stop-mode", "unload", "-state", statePath, "-target", target, command}
			if err := run(); err == nil || !strings.Contains(err.Error(), "requires -target") {
				t.Fatalf("%s %q: %v", command, target, err)
			}
			if _, err := os.Stat(statePath); !os.IsNotExist(err) {
				t.Fatalf("invalid command opened state: %v", err)
			}
		}
	}
	os.Args = []string{"gpu-mode", "-media-stop-mode", "unload", "-target", "text", "status"}
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
	os.Args = []string{"gpu-mode", "-state", filepath.Join(t.TempDir(), "state.db"), "-media-stop-mode", "stop-service", "-text-unit", "text.service", "-media-unit", "media.service", "-text-health-url", "http://127.0.0.1:1/health", "-media-health-url", "http://127.0.0.1:1/health", "-text-cgroup", "/text.service", "-media-cgroup", "/media.service", "-systemctl", "/usr/bin/true", "-nvidia-smi", "/usr/bin/true", "status"}
	if err := run(); err == nil || !strings.Contains(err.Error(), "load state") {
		t.Fatalf("expected observation failure, got %v", err)
	}
}

func TestCLILegacyReleaseThresholdRejectedEvenWhenZero(t *testing.T) {
	previous := os.Args
	t.Cleanup(func() { os.Args = previous })
	for _, value := range []string{"0", "286"} {
		path := filepath.Join(t.TempDir(), "state.db")
		os.Args = []string{"gpu-mode", "-state", path, "-release-max-used-mib=" + value, "restore-state"}
		if err := run(); err == nil || !strings.Contains(err.Error(), "removed") {
			t.Fatalf("legacy flag = %v", err)
		}
		if _, err := os.Stat(path + ".lock"); !os.IsNotExist(err) {
			t.Fatalf("created lock: %v", err)
		}
	}
}

func TestOmittedMediaStopPolicyIsExplicitError(t *testing.T) {
	previous := os.Args
	defer func() { os.Args = previous }()
	os.Args = []string{"gpu-mode", "status"}
	if err := run(); err == nil || !strings.Contains(err.Error(), "media-stop-mode") {
		t.Fatalf("omitted policy: %v", err)
	}
}

func TestPruneAuditCLIRequiresExplicitCutoffAndNoRuntime(t *testing.T) {
	previous, previousOutput := os.Args, os.Stdout
	defer func() { os.Args, os.Stdout = previous, previousOutput }()
	path := filepath.Join(t.TempDir(), "state.db")
	for _, cutoff := range []string{"", "invalid", "2999-01-01T00:00:00Z"} {
		os.Args = []string{"gpu-mode", "-state", path, "-audit-before", cutoff, "prune-audit"}
		if err := run(); err == nil {
			t.Fatal("invalid cutoff accepted")
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("invalid cutoff opened database")
		}
	}
	output, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	os.Stdout = output
	os.Args = []string{"gpu-mode", "-state", path, "-audit-before", "2000-01-01T00:00:00Z", "-audit-batch", "1", "prune-audit"}
	if err := run(); err != nil {
		t.Fatal(err)
	}
	if _, err := output.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	var result map[string]int64
	if err := json.NewDecoder(output).Decode(&result); err != nil || result["prunedAuditRecords"] != 0 {
		t.Fatalf("result %v %v", result, err)
	}
}

func TestAuditFlagsRejectedOutsidePruneBeforeOpeningState(t *testing.T) {
	for _, args := range [][]string{
		{"-audit-before", "2020-01-01T00:00:00Z", "restore-state"},
		{"-audit-batch", "256", "restore-state"},
		{"-audit-batch", "1", "status"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			previous := os.Args
			t.Cleanup(func() { os.Args = previous })
			path := filepath.Join(t.TempDir(), "state.db")
			os.Args = append([]string{"gpu-mode", "-state", path}, args...)
			if err := run(); err == nil || !strings.Contains(err.Error(), "require prune-audit") {
				t.Fatalf("got %v", err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("state created: %v", err)
			}
		})
	}
}
