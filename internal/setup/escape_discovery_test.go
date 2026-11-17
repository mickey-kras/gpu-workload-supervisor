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
	found := []gpuruntime.EscapeRoute{{Route: "cron", Item: "/etc/cron.deny does not deny alice"}}
	backend.escapeRoutes = func(context.Context, gpuruntime.CommandFunc, string) []gpuruntime.EscapeRoute { return found }
	got, err := backend.Discover(context.Background(), home)
	if err != nil || len(got.EscapeRoutes) != 1 {
		t.Fatalf("discovery = %+v, %v: %v", got.EscapeRoutes, err, commands)
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

// The scan has its own budget of escapeScanBudget, also under a caller
// with a far deadline.
func TestEscapeRouteScanHasItsOwnBudget(t *testing.T) {
	var deadline time.Time
	backend := Backend{escapeRoutes: func(ctx context.Context, _ gpuruntime.CommandFunc, _ string) []gpuruntime.EscapeRoute {
		deadline, _ = ctx.Deadline()
		return nil
	}}
	parent, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	start := time.Now()
	backend.discoverEscapeRoutes(parent, &Discovery{})
	if deadline.Before(start.Add(escapeScanBudget-time.Second)) || deadline.After(time.Now().Add(escapeScanBudget)) {
		t.Fatalf("scan deadline %v, start %v", deadline, start)
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
	link := filepath.Join(dir, "link")
	for name, mode := range map[string]os.FileMode{trusted: 0o755, writable: 0o777} {
		if err := os.WriteFile(name, []byte("#!/bin/sh\n"), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(name, mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(trusted, link); err != nil {
		t.Fatal(err)
	}
	var commands, failures, paths []string
	backend := Backend{
		runCommand: func(_ context.Context, name string, _ ...string) ([]byte, error) {
			commands = append(commands, name)
			return nil, nil
		},
		escapeRoutes: func(ctx context.Context, run gpuruntime.CommandFunc, path string) []gpuruntime.EscapeRoute {
			paths = append(paths, path)
			if _, err := run(ctx, path); err != nil {
				failures = append(failures, err.Error())
			}
			return nil
		},
	}
	// A symlink resolves to the trusted target before it runs.
	backend.discoverEscapeRoutes(context.Background(), &Discovery{Request: Request{Profile: Profile{SystemctlPath: link}}})
	if len(commands) != 1 || commands[0] != trusted || paths[0] != trusted || len(failures) != 0 {
		t.Fatalf("trusted systemctl: commands %q, paths %q, failures %q", commands, paths, failures)
	}
	commands = nil
	backend.discoverEscapeRoutes(context.Background(), &Discovery{Request: Request{Profile: Profile{SystemctlPath: writable}}})
	if len(commands) != 0 || len(failures) != 1 ||
		failures[0] != "the command did not run: "+writable+" is not a trusted executable: world-writable; remove other-write or choose another path" {
		t.Fatalf("writable systemctl: commands %q, failures %q", commands, failures)
	}
	var path string
	backend.escapeRoutes = func(_ context.Context, _ gpuruntime.CommandFunc, p string) []gpuruntime.EscapeRoute {
		path = p
		return nil
	}
	// The default path is fixed. Skip this case on a host where it is
	// not a plain file, because the scan then gets another path.
	if info, err := os.Lstat(automaticSystemctlPath); err != nil || !info.Mode().IsRegular() {
		t.Skip("no regular " + automaticSystemctlPath + " on this host")
	}
	backend.discoverEscapeRoutes(context.Background(), &Discovery{})
	if path != automaticSystemctlPath {
		t.Fatalf("default systemctl = %q", path)
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
