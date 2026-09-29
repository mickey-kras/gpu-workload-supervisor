package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/supervisor"
)

const restoreStateCommand = "restore-state"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	flags := flag.NewFlagSet("gpu-mode", flag.ContinueOnError)
	statePath := flags.String("state", defaultStatePath(), "SQLite state path")
	textUnit := flags.String("text-unit", "", "systemd user unit for text inference")
	mediaUnit := flags.String("media-unit", "", "systemd user unit for the media UI")
	textHealth := flags.String("text-health-url", "", "loopback text runtime health URL")
	mediaHealth := flags.String("media-health-url", "", "loopback media runtime health URL")
	mediaRelease := flags.String("media-release-url", "", "loopback media model release URL")
	gpuIndex := flags.Int("gpu-index", 0, "NVIDIA GPU index")
	releaseMaxMiB := flags.Uint64("release-max-used-mib", 0, "maximum used GPU memory after media release")
	nvidiaSMIPath := flags.String("nvidia-smi", "", "absolute path to the trusted nvidia-smi executable")
	systemctlPath := flags.String("systemctl", "", "absolute path to the trusted systemctl executable")
	healthTimeout := flags.Duration("health-timeout", 10*time.Second, "individual health request timeout")
	actionTimeout := flags.Duration("action-timeout", 2*time.Minute, "runtime start or stop timeout")
	drainTimeout := flags.Duration("drain-timeout", 5*time.Minute, "admitted-work drain timeout")
	verifyTimeout := flags.Duration("verify-timeout", 5*time.Minute, "runtime readiness timeout")
	cleanupTimeout := flags.Duration("cleanup-timeout", 2*time.Minute, "failure rollback timeout")
	finalizeTimeout := flags.Duration("finalize-timeout", 10*time.Second, "failure finalization timeout")
	pollInterval := flags.Duration("poll-interval", 250*time.Millisecond, "drain and readiness polling interval")
	resolveReason := flags.String("resolve-reason", "", "required audit reason for resolve-work")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: gpu-mode [flags] restore-state|status|reconcile|recover|resolve-work|text|media|idle")
	}
	command := flags.Arg(0)
	if command != restoreStateCommand && (*textUnit == "" || *mediaUnit == "" || *textHealth == "" || *mediaHealth == "" || *mediaRelease == "" || *releaseMaxMiB == 0 || *nvidiaSMIPath == "" || *systemctlPath == "") {
		return errors.New("runtime units, endpoints, release threshold, and trusted executable paths are required")
	}
	processLock, err := lock.Acquire(*statePath + ".lock")
	if err != nil {
		return err
	}
	defer processLock.Close()
	proxyLock, err := acquireResolutionLock(*statePath, command, *resolveReason)
	if err != nil {
		return err
	}
	defer proxyLock.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var stateStore *store.Store
	if command == restoreStateCommand {
		stateStore, err = store.OpenRestored(ctx, *statePath)
	} else {
		stateStore, err = store.Open(ctx, *statePath)
	}
	if err != nil {
		return fmt.Errorf("open state store: %w", err)
	}
	defer stateStore.Close()
	if command == restoreStateCommand {
		return restoreState(ctx, stateStore)
	}
	runtimeManager, err := gpuruntime.NewSystemdManager(gpuruntime.SystemdConfig{
		TextUnit: *textUnit, MediaUnit: *mediaUnit,
		TextHealthURL: *textHealth, MediaHealthURL: *mediaHealth,
		MediaReleaseURL: *mediaRelease, HealthTimeout: *healthTimeout,
		GPUIndex: *gpuIndex, ReleaseMaxMiB: *releaseMaxMiB,
		NvidiaSMIPath: *nvidiaSMIPath, SystemctlPath: *systemctlPath,
	})
	if err != nil {
		return err
	}
	controller, err := supervisor.New(stateStore, runtimeManager, supervisor.Config{
		DrainTimeout: *drainTimeout, VerifyTimeout: *verifyTimeout,
		ActionTimeout:  *actionTimeout,
		CleanupTimeout: *cleanupTimeout, FinalizeTimeout: *finalizeTimeout,
		PollInterval: *pollInterval,
	})
	if err != nil {
		return err
	}
	return executeCommand(ctx, controller, command, *resolveReason)
}

func acquireResolutionLock(statePath, command, reason string) (*lock.File, error) {
	if command != "resolve-work" {
		return nil, nil
	}
	if reason == "" {
		return nil, errors.New("resolve-work requires -resolve-reason")
	}
	// Proxies hold shared locks until their in-flight handlers finish. Refuse
	// to abandon work while any proxy can still forward an admitted request.
	proxyLock, err := lock.TryAcquire(statePath + ".proxy.lock")
	if err != nil {
		return nil, fmt.Errorf("stop all workload proxies and wait for shutdown before resolve-work: %w", err)
	}
	return proxyLock, nil
}

func executeCommand(ctx context.Context, controller *supervisor.Controller, command, resolveReason string) error {
	var state control.State
	var err error
	switch command {
	case "status":
		state, err = controller.Status(ctx)
	case "reconcile":
		state, err = controller.Reconcile(ctx)
	case "recover":
		state, err = controller.Recover(ctx)
	case "resolve-work":
		var abandoned int64
		state, abandoned, err = controller.ResolveUnfinishedWork(ctx, resolveReason)
		if err == nil {
			return json.NewEncoder(os.Stdout).Encode(struct {
				State         control.State `json:"state"`
				AbandonedWork int64         `json:"abandonedWork"`
			}{state, abandoned})
		}
	case "text":
		state, err = controller.Switch(ctx, control.WorkloadText, "local-cli")
	case "media":
		state, err = controller.Switch(ctx, control.WorkloadMedia, "local-cli")
	case "idle":
		state, err = controller.Switch(ctx, control.WorkloadIdle, "local-cli")
	default:
		return fmt.Errorf("unknown command %q", command)
	}
	if encodeErr := json.NewEncoder(os.Stdout).Encode(state); encodeErr != nil {
		return errors.Join(err, encodeErr)
	}
	return err
}

func restoreState(ctx context.Context, stateStore *store.Store) error {
	restored, err := stateStore.RotateIncarnation(ctx)
	if err != nil {
		return fmt.Errorf("prepare restored state: %w", err)
	}
	return json.NewEncoder(os.Stdout).Encode(restored)
}

func defaultStatePath() string {
	if root := os.Getenv("XDG_STATE_HOME"); root != "" {
		return filepath.Join(root, "gpu-workload-supervisor", "state.db")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "state.db"
	}
	return filepath.Join(home, ".local", "state", "gpu-workload-supervisor", "state.db")
}
