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
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/supervisor"
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
	textUnit := flags.String("text-unit", "llm-server.service", "systemd user unit for text inference")
	mediaUnit := flags.String("media-unit", "comfyui.service", "systemd user unit for media inference")
	textHealth := flags.String("text-health-url", "http://127.0.0.1:8080/health", "text runtime health URL")
	mediaHealth := flags.String("media-health-url", "http://127.0.0.1:8188/", "media runtime health URL")
	healthTimeout := flags.Duration("health-timeout", 10*time.Second, "runtime health timeout")
	drainTimeout := flags.Duration("drain-timeout", 5*time.Minute, "admitted-work drain timeout")
	pollInterval := flags.Duration("poll-interval", 250*time.Millisecond, "drain polling interval")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: gpu-mode [flags] status|reconcile|text|media|idle")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	stateStore, err := store.Open(ctx, *statePath)
	if err != nil {
		return fmt.Errorf("open state store: %w", err)
	}
	defer stateStore.Close()
	runtimeManager, err := gpuruntime.NewSystemdManager(gpuruntime.SystemdConfig{
		TextUnit: *textUnit, MediaUnit: *mediaUnit,
		TextHealthURL: *textHealth, MediaHealthURL: *mediaHealth,
		HealthTimeout: *healthTimeout,
	})
	if err != nil {
		return err
	}
	controller, err := supervisor.New(stateStore, runtimeManager, supervisor.Config{
		DrainTimeout: *drainTimeout,
		PollInterval: *pollInterval,
	})
	if err != nil {
		return err
	}
	var state control.State
	switch flags.Arg(0) {
	case "status":
		state, err = controller.Status(ctx)
	case "reconcile":
		state, err = controller.Reconcile(ctx)
	case "text":
		state, err = controller.Switch(ctx, control.WorkloadText, "local-cli")
	case "media":
		state, err = controller.Switch(ctx, control.WorkloadMedia, "local-cli")
	case "idle":
		state, err = controller.Switch(ctx, control.WorkloadIdle, "local-cli")
	default:
		return fmt.Errorf("unknown command %q", flags.Arg(0))
	}
	if encodeErr := json.NewEncoder(os.Stdout).Encode(state); encodeErr != nil {
		return errors.Join(err, encodeErr)
	}
	return err
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
