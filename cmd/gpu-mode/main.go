package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
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
	statePath, resolveReason, target                                                          string
	auditBatch                                                                                int
	actionTimeout, drainTimeout, verifyTimeout, cleanupTimeout, finalizeTimeout, pollInterval time.Duration
}

func runWithRuntimeFactory(newRuntime func(gpuruntime.SystemdConfig) (gpuruntime.Manager, error)) error {
	flags := flag.NewFlagSet("gpu-mode", flag.ContinueOnError)
	catalogPath := flags.String("catalog", "", "catalog JSON for configure and verify-host")
	catalogRevision := flags.String("configuration-revision", "", "expected accepted catalog revision for configure")
	statePath := flags.String("state", deployment.DefaultStatePath(), "SQLite state path")
	gpuIndex := flags.Int("gpu-index", 0, "NVIDIA GPU index")
	flags.Uint64("release-max-used-mib", 0, "removed: configure workload cgroups and optional target capacity in the catalog")
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
	if err := validateCatalogFlags(flags, *catalogPath); err != nil {
		return err
	}
	if err := validateCommandFlags(flags, *target); err != nil {
		return err
	}
	command := flags.Arg(0)
	auditCutoff, err := validateAuditFlags(command, *auditBefore, *auditBatch)
	if err != nil {
		return err
	}
	runtimeConfig := gpuruntime.SystemdConfig{
		HealthTimeout: *healthTimeout,
		GPUIndex:      *gpuIndex, CapacityHeadroomMiB: *headroom,
		NvidiaSMIPath: *nvidiaSMIPath, SystemctlPath: *systemctlPath,
	}
	if command == "verify-host" {
		catalog, err := decodeCatalogFile(*catalogPath)
		if err != nil {
			return err
		}
		runtimeConfig.Catalog = &catalog
		return verifyHost(runtimeConfig, *actionTimeout, newRuntime)
	}
	return executeWithState(newRuntime, runtimeConfig, command, auditCutoff, modeExecution{
		catalogPath: *catalogPath, catalogRevision: *catalogRevision,
		statePath: *statePath, resolveReason: *resolveReason, target: *target,
		auditBatch: *auditBatch, actionTimeout: *actionTimeout, drainTimeout: *drainTimeout,
		verifyTimeout: *verifyTimeout, cleanupTimeout: *cleanupTimeout,
		finalizeTimeout: *finalizeTimeout, pollInterval: *pollInterval,
	})
}

// The accepted catalog is the only workload configuration model: configure and
// verify-host take a candidate file, every other runtime command pins the
// durably accepted catalog from state.
func validateCatalogFlags(flags *flag.FlagSet, catalogPath string) error {
	switch flags.Arg(0) {
	case "configure", "verify-host":
		if catalogPath == "" {
			return fmt.Errorf("%s requires -catalog", flags.Arg(0))
		}
	default:
		if catalogPath != "" {
			return errors.New("-catalog is only accepted by configure and verify-host; runtime commands use the accepted catalog")
		}
	}
	return nil
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
	pinned, err := pinCatalog(snapshot)
	if err != nil {
		return err
	}
	runtimeConfig.Catalog = &pinned.Catalog
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

func validateCommandFlags(flags *flag.FlagSet, target string) error {
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
		return errors.New("-release-max-used-mib has been removed: configure workload cgroups in the catalog; optional target capacity uses profile requiredMiB plus -capacity-headroom-mib")
	}
	if flags.NArg() != 1 {
		return errors.New("usage: gpu-mode [flags] configure|switch|restore-state|prune-audit|verify-host|status|reconcile|recover|resolve-work|take-control|user-switch|return-control|recover-user")
	}
	if auditFlag && flags.Arg(0) != pruneAuditCommand {
		return errors.New("-audit-before and -audit-batch require prune-audit")
	}
	return validateTarget(flags.Arg(0), target)
}

func acquireProxyLifetimeLock(statePath, command, reason string) (*lock.File, error) {
	switch command {
	case "resolve-work":
		reason = strings.TrimSpace(reason)
		if reason == "" {
			return nil, errors.New("resolve-work requires -resolve-reason")
		}
		if _, err := store.ValidateResolutionReason(reason); err != nil {
			return nil, err
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

func decodeCatalogFile(path string) (control.Catalog, error) {
	file, err := os.Open(path)
	if err != nil {
		return control.Catalog{}, err
	}
	defer file.Close()
	return control.DecodeCatalog(file)
}

func configureCatalog(ctx context.Context, stateStore *store.Store, options modeExecution) error {
	catalog, err := decodeCatalogFile(options.catalogPath)
	if err != nil {
		return err
	}
	accepted, err := stateStore.ReplaceCatalog(ctx, options.catalogRevision, catalog)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(accepted)
}

func pinCatalog(snapshot control.CatalogSnapshot) (*control.CatalogSnapshot, error) {
	if snapshot.Revision == "" {
		return nil, errors.New("no catalog has been accepted; run configure -catalog first")
	}
	return &snapshot, nil
}
