package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

type hostProbeRuntime struct {
	stoppedRecoveryRuntime
	probe func(context.Context) error
}

func (r *hostProbeRuntime) Preflight(ctx context.Context) error { return r.probe(ctx) }

func TestVerifyHostDoesNotOpenState(t *testing.T) {
	sentinel := errors.New("unsupported hierarchy")
	for _, scenario := range []string{"success", "failure", "unsupported", "factory failure", "timeout", "invalid timeout"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "state.db")
			// An invalid database proves verify-host never opens or migrates state.
			before := []byte("not a database")
			if err := os.WriteFile(path, before, 0600); err != nil {
				t.Fatal(err)
			}
			oldArgs := os.Args
			t.Cleanup(func() { os.Args = oldArgs })
			timeout := "1s"
			if scenario == "timeout" {
				timeout = "1ms"
			}
			if scenario == "invalid timeout" {
				timeout = "0s"
			}
			os.Args = []string{"gpu-mode", "-state", path, "-text-unit", "text.service", "-media-unit", "media.service", "-text-cgroup", "/workloads/text.service", "-media-cgroup", "/workloads/media.service", "-text-health-url", "http://127.0.0.1:1/", "-media-health-url", "http://127.0.0.1:1/", "-media-stop-mode", "stop-service", "-systemctl", "/usr/bin/true", "-action-timeout", timeout, "verify-host"}
			called := false
			runtime := &hostProbeRuntime{probe: func(ctx context.Context) error {
				called = true
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > time.Second {
					t.Fatal("host probe has no action deadline")
				}
				if scenario == "failure" {
					return sentinel
				}
				if scenario == "timeout" {
					<-ctx.Done()
					return ctx.Err()
				}
				return nil
			}}
			err := runWithRuntimeFactory(func(gpuruntime.SystemdConfig) (gpuruntime.Manager, error) {
				if scenario == "factory failure" {
					return nil, sentinel
				}
				if scenario == "unsupported" {
					return &stoppedRecoveryRuntime{}, nil
				}
				return runtime, nil
			})
			if (err == nil) != (scenario == "success") {
				t.Fatalf("verify-host: %v", err)
			}
			if (scenario == "failure" || scenario == "factory failure") && !errors.Is(err, sentinel) {
				t.Fatalf("lost cause: %v", err)
			}
			if scenario == "timeout" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("lost deadline: %v", err)
			}
			if called != (scenario == "success" || scenario == "failure" || scenario == "timeout") {
				t.Fatalf("probe called: %v", called)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(before) {
				t.Fatalf("state changed: %q %v", after, err)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 1 {
				t.Fatalf("created state or locks: %v %v", entries, err)
			}
		})
	}
}
