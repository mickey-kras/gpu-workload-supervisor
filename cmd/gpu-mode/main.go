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
	"strings"
	"syscall"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/supervisor"
)

const (
	restoreStateCommand = "restore-state"
	localCLIInitiator   = "local-cli"
)

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
	mediaStopMode := flags.String("media-stop-mode", string(gpuruntime.MediaStopUnload), "media stop policy: unload or stop-service")
	mediaRelease := flags.String("media-release-url", "", "loopback media model release URL")
	gpuIndex := flags.Int("gpu-index", 0, "NVIDIA GPU index")
	flags.Uint64("release-max-used-mib", 0, "removed: configure workload cgroups and optional target capacity")
	textCgroup := flags.String("text-cgroup", "", "text unit cgroup path within /sys/fs/cgroup")
	mediaCgroup := flags.String("media-cgroup", "", "media unit cgroup path within /sys/fs/cgroup")
	textRequired := flags.Uint64("text-required-mib", 0, "measured text VRAM requirement; zero disables its capacity check")
	mediaRequired := flags.Uint64("media-required-mib", 0, "measured media VRAM requirement; zero disables its capacity check")
	headroom := flags.Uint64("capacity-headroom-mib", 0, "additional VRAM headroom for configured target capacity checks")
	nvidiaSMIPath := flags.String("nvidia-smi", "", "absolute path to the trusted nvidia-smi executable")
	systemctlPath := flags.String("systemctl", "", "absolute path to the trusted systemctl executable")
	healthTimeout := flags.Duration("health-timeout", 10*time.Second, "individual health request timeout")
	actionTimeout := flags.Duration("action-timeout", 2*time.Minute, "runtime start or stop timeout")
	drainTimeout := flags.Duration("drain-timeout", 5*time.Minute, "admitted-work drain timeout")
	verifyTimeout := flags.Duration("verify-timeout", 5*time.Minute, "runtime readiness timeout")
	cleanupTimeout := flags.Duration("cleanup-timeout", 2*time.Minute, "failure rollback timeout")
	finalizeTimeout := flags.Duration("finalize-timeout", 10*time.Second, "failure finalization timeout")
	pollInterval := flags.Duration("poll-interval", 250*time.Millisecond, "drain and readiness polling interval")
	target := flags.String("target", "", "required workload for ownership commands: text, media, idle")
	resolveReason := flags.String("resolve-reason", "", "required audit reason for resolve-work")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	legacyRelease := false
	flags.Visit(func(value *flag.Flag) {
		if value.Name == "release-max-used-mib" {
			legacyRelease = true
		}
	})
	if legacyRelease {
		return errors.New("-release-max-used-mib has been removed: remove it and configure -text-cgroup and -media-cgroup; optional target capacity uses -text-required-mib/-media-required-mib plus -capacity-headroom-mib")
	}
	if flags.NArg() != 1 {
		return errors.New("usage: gpu-mode [flags] restore-state|status|reconcile|recover|resolve-work|text|media|idle|take-control|user-switch|return-control|recover-user")
	}
	stopMode := gpuruntime.MediaStopMode(*mediaStopMode)
	if *mediaStopMode == "" {
		return errors.New("media stop mode must not be empty")
	}
	if err := stopMode.Validate(); err != nil {
		return err
	}
	command := flags.Arg(0)
	if err := validateTarget(command, *target); err != nil {
		return err
	}
	runtimeConfig := gpuruntime.SystemdConfig{
		MediaStopMode: stopMode,
		TextUnit:      *textUnit, MediaUnit: *mediaUnit,
		TextHealthURL: *textHealth, MediaHealthURL: *mediaHealth,
		MediaReleaseURL: *mediaRelease, HealthTimeout: *healthTimeout,
		GPUIndex: *gpuIndex, TextCgroup: *textCgroup, MediaCgroup: *mediaCgroup,
		TextRequiredMiB: *textRequired, MediaRequiredMiB: *mediaRequired, CapacityHeadroomMiB: *headroom,
		NvidiaSMIPath: *nvidiaSMIPath, SystemctlPath: *systemctlPath,
	}
	if err := validateRuntimeFlags(command, runtimeConfig); err != nil {
		return err
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
	runtimeManager, err := gpuruntime.NewSystemdManager(runtimeConfig)
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
	return executeCommand(ctx, controller, command, *resolveReason, control.Workload(*target))
}

func validateRuntimeFlags(command string, config gpuruntime.SystemdConfig) error {
	if command == restoreStateCommand {
		return nil
	}
	if config.TextUnit == "" || config.MediaUnit == "" || config.TextHealthURL == "" || config.MediaHealthURL == "" || (config.MediaStopMode != gpuruntime.MediaStopService && config.MediaReleaseURL == "") || config.TextCgroup == "" || config.MediaCgroup == "" || config.SystemctlPath == "" {
		return errors.New("runtime units, endpoints, cgroup paths, and trusted systemctl path are required")
	}
	return nil
}

func acquireResolutionLock(statePath, command, reason string) (*lock.File, error) {
	if command != "resolve-work" {
		return nil, nil
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, errors.New("resolve-work requires -resolve-reason")
	}
	if len(reason) > 512 {
		return nil, errors.New("resolution reason must contain 1 to 512 bytes")
	}
	// Proxies hold shared locks until their in-flight handlers finish. Refuse
	// to abandon work while any proxy can still forward an admitted request.
	proxyLock, err := lock.TryAcquire(statePath + ".proxy.lock")
	if err != nil {
		return nil, fmt.Errorf("stop all workload proxies and wait for shutdown before resolve-work: %w", err)
	}
	return proxyLock, nil
}

func executeCommand(ctx context.Context, controller *supervisor.Controller, command, resolveReason string, target control.Workload) error {
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
	case "take-control":
		state, err = controller.TransferToUser(ctx, target, localCLIInitiator)
	case "user-switch":
		state, err = controller.SwitchUser(ctx, target, localCLIInitiator)
	case "return-control":
		state, err = controller.TransferToSupervisor(ctx, target, localCLIInitiator)
	case "recover-user":
		state, err = controller.RecoverUser(ctx, target, localCLIInitiator)
	case "text":
		state, err = controller.Switch(ctx, control.WorkloadText, localCLIInitiator)
	case "media":
		state, err = controller.Switch(ctx, control.WorkloadMedia, localCLIInitiator)
	case "idle":
		state, err = controller.Switch(ctx, control.WorkloadIdle, localCLIInitiator)
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

func validateTarget(command, target string) error {
	switch command {
	case "take-control", "user-switch", "return-control", "recover-user":
		if target != "text" && target != "media" && target != "idle" {
			return fmt.Errorf("%s requires -target text|media|idle", command)
		}
	default:
		if target != "" {
			return fmt.Errorf("-target is not supported for %s", command)
		}
	}
	return nil
}
