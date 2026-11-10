package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"
	"time"

	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/supervisor"
)

// verifyHost deliberately runs before acquiring locks or opening SQLite.
func verifyHost(config gpuruntime.SystemdConfig, timeout time.Duration, newRuntime func(gpuruntime.SystemdConfig) (supervisor.Runtime, error)) error {
	if timeout <= 0 {
		return errors.New("action timeout must be greater than zero")
	}
	manager, err := newRuntime(config)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	probe, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return manager.Preflight(probe)
}
