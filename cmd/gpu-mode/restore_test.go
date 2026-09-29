package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

func TestRestoreStateCommandNeedsNoRuntimeConfiguration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	stateStore, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	before, err := stateStore.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	before.Owner = control.OwnerUser
	before.DesiredWorkload = control.WorkloadText
	before.ActiveWorkload = control.WorkloadText
	before.Phase = control.PhaseStable
	before.Admission = control.AdmissionOpen
	before, err = stateStore.UpdateState(ctx, before.Version, before)
	if err != nil {
		t.Fatal(err)
	}
	if err := stateStore.Close(); err != nil {
		t.Fatal(err)
	}

	output, err := os.CreateTemp(t.TempDir(), "restore-output")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	previousArgs, previousStdout := os.Args, os.Stdout
	t.Cleanup(func() { os.Args, os.Stdout = previousArgs, previousStdout })
	os.Args = []string{"gpu-mode", "-state", path, "restore-state"}
	os.Stdout = output
	if err := run(); err != nil {
		t.Fatal(err)
	}
	if _, err := output.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	var restored control.State
	if err := json.NewDecoder(output).Decode(&restored); err != nil {
		t.Fatal(err)
	}
	if restored.LeaseFence.Incarnation == before.LeaseFence.Incarnation ||
		restored.Admission != control.AdmissionClosed ||
		restored.Owner != control.OwnerSupervisor ||
		restored.Phase != control.PhaseReconciling {
		t.Fatalf("unsafe restored state: %#v", restored)
	}
	reopened, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	persisted, err := reopened.State(ctx)
	if err != nil || persisted != restored {
		t.Fatalf("printed and persisted states differ: %#v, %#v, %v", restored, persisted, err)
	}
}

func TestRestoreStateRejectsMissingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	previousArgs := os.Args
	t.Cleanup(func() { os.Args = previousArgs })
	os.Args = []string{"gpu-mode", "-state", path, "restore-state"}
	if err := run(); err == nil || !strings.Contains(err.Error(), "must already exist") {
		t.Fatalf("missing backup error = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("missing database was created: %v", err)
	}
}
