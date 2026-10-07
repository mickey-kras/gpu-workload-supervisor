package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

// seedCatalog durably accepts a two-workload catalog through the configure
// command, as every runtime command now requires an accepted catalog.
func seedCatalog(t *testing.T, statePath string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.json")
	raw := `{"version":1,"profiles":[` +
		`{"id":"text","label":"Text","adapter":"systemd","unit":"text.service","cgroup":"/workloads/text.service","healthURL":"http://127.0.0.1:1/health"},` +
		`{"id":"media","label":"Media","adapter":"systemd","unit":"media.service","cgroup":"/workloads/media.service","healthURL":"http://127.0.0.1:1/"}]}`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	previousArgs, previousStdout := os.Args, os.Stdout
	defer func() { os.Args, os.Stdout = previousArgs, previousStdout }()
	out, err := os.CreateTemp(dir, "configure-output")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	os.Args, os.Stdout = []string{"gpu-mode", "-state", statePath, "-catalog", path, "configure"}, out
	if err := run(); err != nil {
		t.Fatal(err)
	}
}

func TestCLIRejectsMissingConfigurationAndUnknownCommand(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"invalid flag", []string{"gpu-mode", "-unknown"}, "flag provided but not defined"},
		{"missing command", []string{"gpu-mode"}, "usage:"},
		{"missing accepted catalog", []string{"gpu-mode", "-state", filepath.Join(t.TempDir(), "state.db"), "status"}, "no catalog has been accepted"},
		{"configure missing catalog", []string{"gpu-mode", "-state", filepath.Join(t.TempDir(), "state.db"), "configure"}, "configure requires -catalog"},
		{"verify-host missing catalog", []string{"gpu-mode", "verify-host"}, "verify-host requires -catalog"},
		{"catalog rejected for runtime command", []string{"gpu-mode", "-state", filepath.Join(t.TempDir(), "state.db"), "-catalog", filepath.Join(t.TempDir(), "catalog.json"), "status"}, "-catalog is only accepted"},
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
	previousArgs, previousStdout := os.Args, os.Stdout
	t.Cleanup(func() { os.Args, os.Stdout = previousArgs, previousStdout })
	statePath := filepath.Join(dir, "state.db")
	seedCatalog(t, statePath)
	args := []string{"gpu-mode", "-state", statePath, "-systemctl", "/usr/bin/true"}
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

func TestCLICommandsFailClosedWhenSystemdCannotBeObserved(t *testing.T) {
	previousArgs, previousStdout := os.Args, os.Stdout
	t.Cleanup(func() { os.Args, os.Stdout = previousArgs, previousStdout })
	for _, command := range []string{"reconcile", "recover", "switch", "take-control", "user-switch", "return-control", "recover-user"} {
		t.Run(command, func(t *testing.T) {
			statePath := filepath.Join(t.TempDir(), "state.db")
			seedCatalog(t, statePath)
			args := []string{"gpu-mode", "-state", statePath, "-systemctl", "/usr/bin/true"}
			if command != "reconcile" && command != "recover" {
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

func TestCLIPositionalWorkloadShortcutsAreRemoved(t *testing.T) {
	previousArgs := os.Args
	t.Cleanup(func() { os.Args = previousArgs })
	for _, shortcut := range []string{"text", "media", "idle"} {
		t.Run(shortcut, func(t *testing.T) {
			statePath := filepath.Join(t.TempDir(), "state.db")
			seedCatalog(t, statePath)
			os.Args = []string{"gpu-mode", "-state", statePath, "-systemctl", "/usr/bin/true", shortcut}
			if err := run(); err == nil || !strings.Contains(err.Error(), "unknown command") {
				t.Fatalf("positional shortcut = %v", err)
			}
		})
	}
}

func TestCLIRejectsInvalidControllerTimeout(t *testing.T) {
	previousArgs := os.Args
	t.Cleanup(func() { os.Args = previousArgs })
	statePath := filepath.Join(t.TempDir(), "state.db")
	seedCatalog(t, statePath)
	os.Args = []string{"gpu-mode", "-state", statePath, "-systemctl", "/usr/bin/true", "-action-timeout", "0s", "status"}
	if err := run(); err == nil || !strings.Contains(err.Error(), "timeouts") {
		t.Fatalf("invalid controller timeout = %v", err)
	}
}

func TestCLIWorkResolutionRequiresReasonAndStoppedProxies(t *testing.T) {
	previousArgs := os.Args
	previousStdout := os.Stdout
	t.Cleanup(func() { os.Args, os.Stdout = previousArgs, previousStdout })
	statePath := filepath.Join(t.TempDir(), "state.db")
	seedCatalog(t, statePath)
	args := []string{"gpu-mode", "-state", statePath, "-systemctl", "/usr/bin/true"}
	os.Args = append(append([]string{}, args...), "resolve-work")
	if err := run(); err == nil || !strings.Contains(err.Error(), "requires -resolve-reason") {
		t.Fatalf("missing reason error = %v", err)
	}
	// Length constraints are covered by the store; the CLI maps a blank reason
	// to its own missing-flag error.
	os.Args = append(append([]string{}, args...), "-resolve-reason", " \t\n ", "resolve-work")
	if err := run(); err == nil || !strings.Contains(err.Error(), "requires -resolve-reason") {
		t.Fatalf("blank reason error = %v", err)
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
		for _, target := range []string{"", "unknown", "INVALID"} {
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

func TestCLIServiceStopAdapterDoesNotRequireReleaseEndpoint(t *testing.T) {
	previous := os.Args
	t.Cleanup(func() { os.Args = previous })
	statePath := filepath.Join(t.TempDir(), "state.db")
	seedCatalog(t, statePath)
	os.Args = []string{"gpu-mode", "-state", statePath, "-systemctl", "/usr/bin/true", "status"}
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

func TestRemovedLegacyWorkloadFlagsAreRejectedBeforeOpeningState(t *testing.T) {
	previous := os.Args
	t.Cleanup(func() { os.Args = previous })
	for _, flag := range []string{"-workload", "-text-unit", "-media-unit", "-text-health-url", "-media-health-url", "-media-stop-mode", "-media-release-url", "-text-cgroup", "-media-cgroup", "-text-required-mib", "-media-required-mib", "-configured"} {
		path := filepath.Join(t.TempDir(), "state.db")
		os.Args = []string{"gpu-mode", "-state", path, flag, "x", "status"}
		if err := run(); err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
			t.Fatalf("%s: %v", flag, err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s created state: %v", flag, err)
		}
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

func TestCLIShowSettingsPrintsCommittedIdlePolicy(t *testing.T) {
	previous, previousOutput := os.Args, os.Stdout
	defer func() { os.Args, os.Stdout = previous, previousOutput }()
	path := filepath.Join(t.TempDir(), "state.db")
	os.Args = []string{"gpu-mode", "-state", path, "-target", "text", "show-settings"}
	if err := run(); err == nil || !strings.Contains(err.Error(), "-target is not supported") {
		t.Fatalf("target accepted for show-settings: %v", err)
	}
	output, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	os.Stdout = output
	os.Args = []string{"gpu-mode", "-state", path, "show-settings"}
	if err := run(); err != nil {
		t.Fatal(err)
	}
	if _, err := output.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Policy           control.IdlePolicy `json:"policy"`
		SettingsRevision string             `json:"settingsRevision"`
		LastActivityAt   *time.Time         `json:"lastActivityAt"`
		ArmedDeadline    *time.Time         `json:"armedDeadline"`
		AttestationAt    *time.Time         `json:"attestationAt"`
	}
	if err := json.NewDecoder(output).Decode(&result); err != nil {
		t.Fatalf("show-settings output is not JSON: %v", err)
	}
	if result.Policy.TimeoutMinutes != 0 {
		t.Fatalf("seeded policy = %#v, want Off", result.Policy)
	}
	if result.SettingsRevision == "" {
		t.Fatal("settings revision is empty")
	}
	if result.LastActivityAt == nil || result.ArmedDeadline != nil || result.AttestationAt != nil {
		t.Fatalf("seeded policy state = %+v", result)
	}
}

func TestCLIIdlePolicyTick(t *testing.T) {
	previous := os.Args
	defer func() { os.Args = previous }()

	t.Run("off policy is a clean no-op", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.db")
		seedCatalog(t, path)
		os.Args = []string{"gpu-mode", "-state", path, "-systemctl", "/usr/bin/true", "idle-policy-tick"}
		if err := run(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("enabled policy fails closed without evidence", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.db")
		seedCatalog(t, path)
		ctx := context.Background()
		s, err := store.Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		state, err := s.State(ctx)
		if err != nil {
			t.Fatal(err)
		}
		state.Phase = control.PhaseStable
		state.DesiredWorkload = control.WorkloadText
		state.ActiveWorkload = control.WorkloadText
		state.Admission = control.AdmissionOpen
		state, err = s.UpdateState(ctx, state.Version, state)
		if err != nil {
			t.Fatal(err)
		}
		snap, err := s.Catalog(ctx)
		if err != nil {
			t.Fatal(err)
		}
		settings, err := s.Settings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		e := control.SettingsPrecondition{
			Incarnation: state.LeaseFence.Incarnation, Version: state.Version, Owner: state.Owner,
			ConfigurationRevision: snap.Revision, SettingsRevision: settings.SettingsRevision,
		}
		if _, err := s.SetIdlePolicy(ctx, e, control.IdlePolicy{TimeoutMinutes: 5}, true); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		os.Args = []string{"gpu-mode", "-state", path, "-systemctl", "/usr/bin/true", "idle-policy-tick"}
		if err := run(); err == nil || !strings.Contains(err.Error(), "evidence") {
			t.Fatalf("enabled tick without evidence: %v", err)
		}
	})
}
