package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

func TestManagedDeploymentRefusesBeforeDatabaseOpen(t *testing.T) {
	for _, marker := range []deployment.Marker{{Version: 1, Release: deployment.Release}, {Version: 1, Release: deployment.Release, Maintenance: true}, {Version: 1, Release: "unactivated"}} {
		path := filepath.Join(t.TempDir(), "state.db")
		if err := deployment.Write(path, marker); err != nil {
			t.Fatal(err)
		}
		called := false
		err := executeWithState(func(gpuruntime.SystemdConfig) (gpuruntime.Manager, error) {
			called = true
			return nil, errors.New("unexpected runtime")
		}, gpuruntime.SystemdConfig{}, "status", time.Time{}, modeExecution{statePath: path})
		if err == nil || called {
			t.Fatalf("guard failed: %v %v", err, called)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("guard opened state", err)
		}
	}
}
func TestLegacyRelativeStateWithoutMarkerStillOpens(t *testing.T) {
	t.Chdir(t.TempDir())
	called := false
	sentinel := errors.New("runtime reached")
	err := executeWithState(func(gpuruntime.SystemdConfig) (gpuruntime.Manager, error) { called = true; return nil, sentinel }, gpuruntime.SystemdConfig{}, "status", time.Time{}, modeExecution{statePath: "state.db"})
	if !called || !errors.Is(err, sentinel) {
		t.Fatalf("legacy relative path failed: %v", err)
	}
	if _, err := os.Stat("state.db"); err != nil {
		t.Fatal(err)
	}
}
