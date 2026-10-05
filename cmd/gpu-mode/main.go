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
	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/supervisor"
)

const (
	restoreStateCommand = "restore-state"
	pruneAuditCommand   = "prune-audit"
	localCLIInitiator   = "local-cli"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	return runWithRuntimeFactory(func(config gpuruntime.SystemdConfig) (gpuruntime.Manager, error) {
		return gpuruntime.NewSystemdManager(config)
	})
}

type modeExecution struct {
	catalogPath, catalogRevision                                                              string
	configured                                                                                bool
	legacyConfig                                                                              bool
	statePath, resolveReason, target                                                          string
	auditBatch                                                                                int
	actionTimeout, drainTimeout, verifyTimeout, cleanupTimeout, finalizeTimeout, pollInterval time.Duration
}

func runWithRuntimeFactory(newRuntime func(gpuruntime.SystemdConfig) (gpuruntime.Manager, error)) error {
	flags := flag.NewFlagSet("gpu-mode", flag.ContinueOnError)
	catalogPath := flags.String("catalog", "", "catalog JSON for explicit configure command")
	catalogRevision := flags.String("configuration-revision", "", "expected accepted catalog revision for configure")
	configured := flags.Bool("configured", false, "use the durably accepted workload catalog")
	workload := flags.String("workload", "", "configured workload ID for switch")
	statePath := flags.String("state", defaultStatePath(), "SQLite state path")
	textUnit := flags.String("text-unit", "", "systemd user unit for text inference")
	mediaUnit := flags.String("media-unit", "", "systemd user unit for the media UI")
	textHealth := flags.String("text-health-url", "", "loopback text runtime health URL")
	mediaHealth := flags.String("media-health-url", "", "loopback media runtime health URL")
	mediaStopMode := flags.String("media-stop-mode", "", "media stop policy: unload or stop-service")
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
	healthTimeout := flags.Duration("health-timeout", 10*time.Second, "complete runtime health probe timeout")
	actionTimeout := flags.Duration("action-timeout", 2*time.Minute, "runtime action or observation probe timeout")
	drainTimeout := flags.Duration("drain-timeout", 5*time.Minute, "admitted-work drain timeout")
	verifyTimeout := flags.Duration("verify-timeout", 5*time.Minute, "runtime readiness timeout")
	cleanupTimeout := flags.Duration("cleanup-timeout", 2*time.Minute, "failure rollback timeout")
	finalizeTimeout := flags.Duration("finalize-timeout", 10*time.Second, "durable transition finalization timeout")
	pollInterval := flags.Duration("poll-interval", 250*time.Millisecond, "drain and readiness polling interval")
	target := flags.String("target", "", "required workload ID for ownership commands; idle selects no workload")
	auditBefore := flags.String("audit-before", "", "prune-audit cutoff (RFC3339); archive first")
	auditBatch := flags.Int("audit-batch", 256, "maximum audit parents removed by prune-audit (1..1024)")
	resolveReason := flags.String("resolve-reason", "", "required audit reason for resolve-work")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	legacyConfig, err := validateCatalogFlags(flags, *workload, target, *configured, *catalogPath)
	if err != nil {
		return err
	}
	stopMode, err := validateCommandFlags(flags, *mediaStopMode, *target)
	if err != nil {
		return err
	}
	command := flags.Arg(0)
	auditCutoff, err := validateAuditFlags(command, *auditBefore, *auditBatch)
	if err != nil {
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
	if !*configured && command != "configure" {
		if err := validateRuntimeFlags(command, runtimeConfig); err != nil {
			return err
		}
	}
	if command == "verify-host" {
		return verifyHost(runtimeConfig, *actionTimeout, newRuntime)
	}
	return executeWithState(newRuntime, runtimeConfig, command, auditCutoff, modeExecution{
		catalogPath: *catalogPath, catalogRevision: *catalogRevision, configured: *configured, legacyConfig: legacyConfig,
		statePath: *statePath, resolveReason: *resolveReason, target: *target,
		auditBatch: *auditBatch, actionTimeout: *actionTimeout, drainTimeout: *drainTimeout,
		verifyTimeout: *verifyTimeout, cleanupTimeout: *cleanupTimeout,
		finalizeTimeout: *finalizeTimeout, pollInterval: *pollInterval,
	})
}

func validateCatalogFlags(flags *flag.FlagSet, workload string, target *string, configured bool, catalogPath string) (bool, error) {
	if workload != "" {
		if *target != "" || flags.Arg(0) != "switch" {
			return false, errors.New("-workload requires switch and conflicts with -target")
		}
		*target = workload
	}
	legacyConfig := false
	flags.Visit(func(f *flag.Flag) {
		if strings.HasPrefix(f.Name, "text-") || strings.HasPrefix(f.Name, "media-") {
			legacyConfig = true
		}
	})
	if (configured || catalogPath != "") && legacyConfig {
		return false, errors.New("catalog configuration conflicts with legacy workload flags")
	}
	if catalogPath != "" && flags.Arg(0) != "configure" {
		return false, errors.New("-catalog is only accepted by configure; use -configured for runtime commands")
	}
	if flags.Arg(0) == "configure" && catalogPath == "" {
		return false, errors.New("configure requires -catalog")
	}
	return legacyConfig, nil
}

func executeWithState(newRuntime func(gpuruntime.SystemdConfig) (gpuruntime.Manager, error), runtimeConfig gpuruntime.SystemdConfig, command string, auditCutoff time.Time, options modeExecution) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	processLock, err := lock.AcquireContext(ctx, options.statePath+".lock", false)
	if err != nil {
		return err
	}
	defer processLock.Close()
	proxyLock, err := acquireProxyLifetimeLock(options.statePath, command, options.resolveReason)
	if err != nil {
		return err
	}
	defer proxyLock.Close()
	if err := deployment.Check(options.statePath, ""); err != nil {
		return err
	}
	var stateStore *store.Store
	if command == restoreStateCommand {
		stateStore, err = store.OpenRestored(ctx, options.statePath)
	} else {
		stateStore, err = store.Open(ctx, options.statePath)
	}
	if err != nil {
		return fmt.Errorf("open state store: %w", err)
	}
	defer stateStore.Close()
	if command == restoreStateCommand {
		return restoreState(ctx, stateStore)
	}
	if command == pruneAuditCommand {
		return pruneAudit(ctx, stateStore, auditCutoff, options.auditBatch)
	}
	snapshot, err := stateStore.Catalog(ctx)
	if err != nil {
		return err
	}
	if command == "configure" {
		return configureCatalog(ctx, stateStore, options)
	}
	pinned, err := pinCatalog(snapshot, options)
	if err != nil {
		return err
	}
	if pinned != nil {
		runtimeConfig.Catalog = &pinned.Catalog
	}
	runtimeManager, err := newRuntime(runtimeConfig)
	if err != nil {
		return err
	}
	controller, err := supervisor.New(stateStore, runtimeManager, supervisor.Config{
		Catalog:      pinned,
		DrainTimeout: options.drainTimeout, VerifyTimeout: options.verifyTimeout,
		ActionTimeout:  options.actionTimeout,
		CleanupTimeout: options.cleanupTimeout, FinalizeTimeout: options.finalizeTimeout,
		PollInterval: options.pollInterval,
	})
	if err != nil {
		return err
	}
	return executeCommand(ctx, controller, command, options.resolveReason, control.Workload(options.target))
}

func validateCommandFlags(flags *flag.FlagSet, mediaStopMode, target string) (gpuruntime.MediaStopMode, error) {
	legacyRelease := false
	auditFlag := false
	flags.Visit(func(value *flag.Flag) {
		if value.Name == "audit-before" || value.Name == "audit-batch" {
			auditFlag = true
		}
		if value.Name == "release-max-used-mib" {
			legacyRelease = true
		}
	})
	if legacyRelease {
		return "", errors.New("-release-max-used-mib has been removed: remove it and configure -text-cgroup and -media-cgroup; optional target capacity uses -text-required-mib/-media-required-mib plus -capacity-headroom-mib")
	}
	if flags.NArg() != 1 {
		return "", errors.New("usage: gpu-mode [flags] configure|switch|restore-state|prune-audit|verify-host|status|reconcile|recover|resolve-work|text|media|idle|take-control|user-switch|return-control|recover-user")
	}
	if auditFlag && flags.Arg(0) != pruneAuditCommand {
		return "", errors.New("-audit-before and -audit-batch require prune-audit")
	}
	stopMode := gpuruntime.MediaStopMode(mediaStopMode)
	configuredFlag := flags.Lookup("configured")
	usingCatalog := configuredFlag != nil && configuredFlag.Value.String() == "true"
	if mediaStopMode == "" && !usingCatalog && flags.Arg(0) != "configure" && flags.Arg(0) != restoreStateCommand && flags.Arg(0) != pruneAuditCommand {
		return "", errors.New("media stop mode must not be empty: explicitly select -media-stop-mode unload or stop-service")
	}
	if err := stopMode.Validate(); err != nil {
		return "", err
	}
	command := flags.Arg(0)
	if err := validateTarget(command, target); err != nil {
		return "", err
	}
	return stopMode, nil
}

func validateRuntimeFlags(command string, config gpuruntime.SystemdConfig) error {
	if command == restoreStateCommand || command == pruneAuditCommand {
		return nil
	}
	if config.TextUnit == "" || config.MediaUnit == "" || config.TextHealthURL == "" || config.MediaHealthURL == "" || (config.MediaStopMode != gpuruntime.MediaStopService && config.MediaReleaseURL == "") || config.TextCgroup == "" || config.MediaCgroup == "" || config.SystemctlPath == "" {
		return errors.New("runtime units, endpoints, cgroup paths, and trusted systemctl path are required")
	}
	return nil
}

func acquireProxyLifetimeLock(statePath, command, reason string) (*lock.File, error) {
	switch command {
	case "resolve-work":
		reason = strings.TrimSpace(reason)
		if reason == "" {
			return nil, errors.New("resolve-work requires -resolve-reason")
		}
		if len(reason) > 512 {
			return nil, errors.New("resolution reason must contain 1 to 512 bytes")
		}
	case restoreStateCommand, "configure":
	default:
		return nil, nil
	}
	// Proxies hold shared locks until their in-flight handlers finish. Refuse
	// to abandon work or replace configuration while a proxy can still forward.
	proxyLock, err := lock.TryAcquire(statePath + ".proxy.lock")
	if err != nil {
		return nil, fmt.Errorf("stop all workload proxies and wait for shutdown before %s: %w", command, err)
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
	case "switch":
		state, err = controller.Switch(ctx, target, localCLIInitiator)
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
	case "switch", "take-control", "user-switch", "return-control", "recover-user":
		if !control.ValidWorkloadID(control.Workload(target)) && target != "idle" {
			return fmt.Errorf("%s requires -target with a valid workload ID or idle", command)
		}
	default:
		if target != "" {
			return fmt.Errorf("-target is not supported for %s", command)
		}
	}
	return nil
}

func configureCatalog(ctx context.Context, stateStore *store.Store, options modeExecution) error {
	file, err := os.Open(options.catalogPath)
	if err != nil {
		return err
	}
	defer file.Close()
	catalog, err := control.DecodeCatalog(file)
	if err != nil {
		return err
	}
	accepted, err := stateStore.ReplaceCatalog(ctx, options.catalogRevision, catalog)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(accepted)
}

func pinCatalog(snapshot control.CatalogSnapshot, options modeExecution) (*control.CatalogSnapshot, error) {
	var pinned *control.CatalogSnapshot
	if snapshot.Revision != "" {
		if options.legacyConfig {
			return nil, errors.New("legacy workload flags conflict with accepted catalog")
		}
		pinned = &snapshot
	} else if options.configured {
		return nil, errors.New("no catalog has been accepted")
	}
	return pinned, nil
}
