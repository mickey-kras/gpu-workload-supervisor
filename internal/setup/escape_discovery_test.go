package setup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

// Discovery reports the escape routes as an advisory list. The scan uses
// the backend command runner and the systemctl of the profile. Without the
// scan, the list is null. A scan without findings gives an empty list.
func TestDiscoveryReportsEscapeRoutes(t *testing.T) {
	backend, home, _ := fixture(t)
	var commands []string
	backend.runCommand = func(_ context.Context, name string, args ...string) ([]byte, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	var deadline time.Time
	found := []gpuruntime.EscapeRoute{{Route: "cron", Item: "/etc/cron.deny does not deny alice"}}
	backend.escapeRoutes = func(ctx context.Context, _ gpuruntime.CommandFunc, _ string) []gpuruntime.EscapeRoute {
		deadline, _ = ctx.Deadline()
		return found
	}
	start := time.Now()
	got, err := backend.Discover(context.Background(), home)
	// The scan has its own budget, not the rest of the 15 s of discovery.
	if err != nil || len(got.EscapeRoutes) != 1 || deadline.Before(start) || deadline.After(time.Now().Add(escapeScanBudget)) {
		t.Fatalf("discovery = %+v, %v with deadline %v: %v", got.EscapeRoutes, err, deadline, commands)
	}
	data, err := json.Marshal(got)
	if err != nil || !strings.Contains(string(data), `"escapeRoutes":[{"route":"cron","item":"/etc/cron.deny does not deny alice"}]`) {
		t.Fatalf("discovery JSON = %s, %v", data, err)
	}
	found = []gpuruntime.EscapeRoute{}
	got, err = backend.Discover(context.Background(), home)
	if data, _ := json.Marshal(got); err != nil || !strings.Contains(string(data), `"escapeRoutes":[]`) {
		t.Fatalf("discovery without findings = %s, %v", data, err)
	}
	backend.escapeRoutes = nil
	got, err = backend.Discover(context.Background(), home)
	if data, _ := json.Marshal(got); err != nil || !strings.Contains(string(data), `"escapeRoutes":null`) {
		t.Fatalf("discovery without the scan = %s, %v", data, err)
	}
	if SystemBackend().escapeRoutes == nil {
		t.Fatal("the system backend does not scan the escape routes")
	}
}

// The scan uses the systemctl path of the profile, after the trust check,
// and the backend runner. Without a profile path, it uses the fixed setup
// path. An untrusted systemctl does not run, and the scan gets a runner
// that fails with the reason.
func TestEscapeRouteScanUsesProfileSystemctl(t *testing.T) {
	dir := t.TempDir()
	trusted := filepath.Join(dir, "systemctl")
	writable := filepath.Join(dir, "writable-systemctl")
	for name, mode := range map[string]os.FileMode{trusted: 0o755, writable: 0o777} {
		if err := os.WriteFile(name, []byte("#!/bin/sh\n"), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(name, mode); err != nil {
			t.Fatal(err)
		}
	}
	var commands, failures []string
	backend := Backend{
		runCommand: func(_ context.Context, name string, _ ...string) ([]byte, error) {
			commands = append(commands, name)
			return nil, nil
		},
		escapeRoutes: func(ctx context.Context, run gpuruntime.CommandFunc, path string) []gpuruntime.EscapeRoute {
			if _, err := run(ctx, path); err != nil {
				failures = append(failures, err.Error())
			}
			return nil
		},
	}
	backend.discoverEscapeRoutes(context.Background(), &Discovery{Request: Request{Profile: Profile{SystemctlPath: trusted}}})
	if len(commands) != 1 || commands[0] != trusted || len(failures) != 0 {
		t.Fatalf("trusted systemctl: commands %q, failures %q", commands, failures)
	}
	commands = nil
	backend.discoverEscapeRoutes(context.Background(), &Discovery{Request: Request{Profile: Profile{SystemctlPath: writable}}})
	if len(commands) != 0 || len(failures) != 1 || failures[0] != writable+" is not a trusted executable" {
		t.Fatalf("writable systemctl: commands %q, failures %q", commands, failures)
	}
	var path string
	backend.escapeRoutes = func(_ context.Context, _ gpuruntime.CommandFunc, p string) []gpuruntime.EscapeRoute {
		path = p
		return nil
	}
	backend.discoverEscapeRoutes(context.Background(), &Discovery{})
	want, err := gpuruntime.ValidateSystemctlExecutable(automaticSystemctlPath)
	if err != nil {
		want = automaticSystemctlPath
	}
	if path != want {
		t.Fatalf("default systemctl = %q, want %q", path, want)
	}
}

// A canceled caller still stops the scan.
func TestEscapeRouteScanFollowsTheCaller(t *testing.T) {
	var scanErr error
	backend := Backend{escapeRoutes: func(ctx context.Context, _ gpuruntime.CommandFunc, _ string) []gpuruntime.EscapeRoute {
		scanErr = ctx.Err()
		return nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	backend.discoverEscapeRoutes(ctx, &Discovery{})
	if scanErr == nil {
		t.Fatal("the scan context is not canceled with the caller")
	}
}
