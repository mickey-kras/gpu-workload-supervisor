package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/operator"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/supervisor"
	"golang.org/x/sys/unix"
)

func profileLoader(p operator.Profile, err error) func() (operator.Profile, error) {
	return func() (operator.Profile, error) { return p, err }
}

// gpu-mode uses the reserve of the operator profile for the same state
// path, also for a relative path. A missing profile or another state path
// stores none. A profile that cannot be read gives an error, so the
// runtime fails its limit checks instead of using the default.
func TestLimitReserveFor(t *testing.T) {
	stored := &gpuruntime.LimitReserve{MemoryMiB: 4096, Tasks: 1024, CPUPercent: 200, IOPercent: 20}
	if r, err := limitReserveFor("/state/state.db", profileLoader(operator.Profile{StatePath: "/state/state.db", LimitReserve: stored}, nil)); r != stored || err != nil {
		t.Fatalf("same state path = %+v, %v", r, err)
	}
	dir := t.TempDir()
	t.Chdir(dir)
	if r, err := limitReserveFor("state.db", profileLoader(operator.Profile{StatePath: filepath.Join(dir, "state.db"), LimitReserve: stored}, nil)); r != stored || err != nil {
		t.Fatalf("relative state path = %+v, %v", r, err)
	}
	if r, err := limitReserveFor("/state/state.db", profileLoader(operator.Profile{StatePath: "/other/state.db", LimitReserve: stored}, nil)); r != nil || err != nil {
		t.Fatalf("other state path = %+v, %v", r, err)
	}
	for _, missing := range []error{fs.ErrNotExist, unix.ENOENT} {
		if r, err := limitReserveFor("/state/state.db", profileLoader(operator.Profile{}, missing)); r != nil || err != nil {
			t.Fatalf("no profile (%v) = %+v, %v", missing, r, err)
		}
	}
	if r, err := limitReserveFor("/state/state.db", profileLoader(operator.Profile{}, errors.New("invalid profile fields"))); r != nil || err == nil ||
		!strings.Contains(err.Error(), "operator.json, or a directory above it, cannot be read or is not valid") {
		t.Fatalf("unreadable profile = %+v, %v", r, err)
	}
}

// The runtime of a stateful command gets the stored reserve, read under
// the state lock, and an unreadable profile reaches it as an error.
func TestCommandsUseTheStoredReserve(t *testing.T) {
	oldArgs, oldLoad := os.Args, loadOperatorProfile
	t.Cleanup(func() { os.Args, loadOperatorProfile = oldArgs, oldLoad })
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.db")
	file := filepath.Join(dir, "catalog.json")
	raw := `{"version":1,"profiles":[{"id":"speech","label":"Speech","adapter":"systemd","unit":"speech.service","cgroup":"/workloads/speech.service","healthURL":"http://localhost:9000"}]}`
	if err := os.WriteFile(file, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	var got gpuruntime.SystemdConfig
	factory := func(cfg gpuruntime.SystemdConfig) (supervisor.Runtime, error) {
		got = cfg
		return &cliCatalogRuntime{}, nil
	}
	invoke := func(args ...string) error {
		os.Args = append([]string{"gpu-mode", "-state", statePath}, args...)
		return runWithRuntimeFactory(factory)
	}
	stored := &gpuruntime.LimitReserve{MemoryMiB: 4096, Tasks: 1024, CPUPercent: 200, IOPercent: 20}
	loadOperatorProfile = profileLoader(operator.Profile{StatePath: statePath, LimitReserve: stored}, nil)
	if err := invoke("-catalog", file, "configure"); err != nil {
		t.Fatal(err)
	}
	if err := invoke("reconcile"); err != nil || got.LimitReserve != stored || got.LimitReserveErr != nil {
		t.Fatalf("reconcile runtime reserve = %+v, %v: %v", got.LimitReserve, got.LimitReserveErr, err)
	}
	loadOperatorProfile = profileLoader(operator.Profile{}, errors.New("untrusted profile"))
	if err := invoke("-catalog", file, "verify-host"); err != nil || got.LimitReserveErr == nil {
		t.Fatalf("verify-host with an unreadable profile = %v, %v", got.LimitReserveErr, err)
	}
	got = gpuruntime.SystemdConfig{}
	if err := invoke("reconcile"); err != nil || got.LimitReserveErr == nil || got.LimitReserve != nil {
		t.Fatalf("reconcile with an unreadable profile = %v, %v", got.LimitReserveErr, err)
	}
	loadOperatorProfile = profileLoader(operator.Profile{}, fs.ErrNotExist)
	got = gpuruntime.SystemdConfig{}
	if err := invoke("reconcile"); err != nil || got.LimitReserveErr != nil || got.LimitReserve != nil {
		t.Fatalf("reconcile without a profile = %v, %v", got.LimitReserveErr, err)
	}
}
