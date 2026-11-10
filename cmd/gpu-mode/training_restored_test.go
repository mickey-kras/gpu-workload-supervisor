package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
)

type restoredRecoveryLine struct {
	ID        string `json:"trainingJob"`
	Category  string `json:"category"`
	Recovered *int   `json:"recoveredTraining"`
}

func decodeRestoredRecovery(t *testing.T, raw []byte) []restoredRecoveryLine {
	t.Helper()
	var lines []restoredRecoveryLine
	decoder := json.NewDecoder(bytes.NewReader(raw))
	for decoder.More() {
		var line restoredRecoveryLine
		if err := decoder.Decode(&line); err != nil {
			t.Fatalf("output is not JSON lines: %s: %v", raw, err)
		}
		lines = append(lines, line)
	}
	return lines
}

// -all-restored recovers each reserved job and preparing claim of restored
// state, as the documented query and per-ID recovery did, and leaves
// nothing for a second run.
func TestRecoverAllRestoredTraining(t *testing.T) {
	ctx := context.Background()
	statePath := filepath.Join(t.TempDir(), "state.db")
	c, s, r := cliTrainingFixtureAt(t, statePath)
	options := modeExecution{trainingJob: "job-a", target: "trainer"}
	if _, err := captureTrainingCommand(t, func() error { return executeRuntimeCommand(ctx, c, trainingStartCommand, options) }); err != nil {
		t.Fatal(err)
	}
	all := modeExecution{allRestored: true, statePath: statePath}
	raw, err := captureTrainingCommand(t, func() error { return executeRuntimeCommand(ctx, c, trainingRecoverCommand, all) })
	if err != nil {
		t.Fatal(err)
	}
	lines := decodeRestoredRecovery(t, raw)
	if len(lines) != 2 || lines[0].ID != "job-a" || lines[0].Category != "reserved job" || lines[1].Recovered == nil || *lines[1].Recovered != 1 {
		t.Fatalf("recovery output = %s", raw)
	}
	if strings.Contains(string(raw), r.terminal.Token) {
		t.Fatal("recovery output leaked the completion token")
	}
	job, err := s.TrainingJob(ctx, "job-a")
	if err != nil || job.Status != "interrupted" {
		t.Fatalf("job after recovery = %+v, %v", job, err)
	}
	snapshot, err := s.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimTraining(ctx, "job-b", "trainer", snapshot.Revision); err != nil {
		t.Fatal(err)
	}
	raw, err = captureTrainingCommand(t, func() error { return executeRuntimeCommand(ctx, c, trainingRecoverCommand, all) })
	if err != nil {
		t.Fatal(err)
	}
	lines = decodeRestoredRecovery(t, raw)
	if len(lines) != 2 || lines[0].ID != "job-b" || lines[0].Category != "preparing claim" {
		t.Fatalf("claim recovery output = %s", raw)
	}
	raw, err = captureTrainingCommand(t, func() error { return executeRuntimeCommand(ctx, c, trainingRecoverCommand, all) })
	if lines = decodeRestoredRecovery(t, raw); err != nil || len(lines) != 1 || *lines[0].Recovered != 0 {
		t.Fatalf("second run = %s, %v", raw, err)
	}
}

func TestRecoverAllRestoredRequiresStoppedProxies(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.db")
	c, _, _ := cliTrainingFixtureAt(t, statePath)
	proxy, err := lock.AcquireContext(context.Background(), statePath+".proxy.lock", true)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	if err := recoverAllRestored(context.Background(), c, statePath); err == nil || !strings.Contains(err.Error(), "stop all workload proxies") {
		t.Fatalf("recovery with a live proxy = %v", err)
	}
}

func TestRecoverAllRestoredReportsAnUnreadableState(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.db")
	c, _, _ := cliTrainingFixtureAt(t, statePath)
	missing := filepath.Join(t.TempDir(), "missing.db")
	if err := recoverAllRestored(context.Background(), c, missing); err == nil || !strings.Contains(err.Error(), "list unfinished training") {
		t.Fatalf("recovery of a missing state = %v", err)
	}
}

func TestAllRestoredFlagNeedsTrainingRecover(t *testing.T) {
	for _, tc := range []struct {
		command, id string
		ok          bool
	}{
		{trainingRecoverCommand, "", true},
		{trainingRecoverCommand, "job-a", false},
		{"training-status", "", false},
		{"status", "", false},
	} {
		if err := validateTrainingFlags(tc.command, tc.id, true); (err == nil) != tc.ok {
			t.Fatalf("%s with ID %q = %v", tc.command, tc.id, err)
		}
	}
}
