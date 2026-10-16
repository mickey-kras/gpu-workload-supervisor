//go:build systemd_integration

package supervisor

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"golang.org/x/sys/unix"
)

// Requires a real user manager and unified cgroups. This exercises systemd's
// finite-job identity/containment contract, not trainer or GPU qualification.
func TestSystemdTrainingRetainsInvocationUntilExplicitStop(t *testing.T) {
	// Keep trusted terminal evidence outside CI worktrees with writable ancestors.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(home, "gws-training-contract-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	f := newSystemdFixture(t)
	command := func(args ...string) string { t.Helper(); return systemdCommand(t, args...) }
	properties := func(unit string) map[string]string {
		t.Helper()
		raw := command("show", "--property=InvocationID,ActiveState,SubState,ControlGroup", "--", unit)
		values, err := gpuruntime.ParseUnitProperties([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		return values
	}
	unit := fmt.Sprintf("gws-training-contract-%d-%d.service", os.Getpid(), time.Now().UnixNano())
	binding := &control.TrainerBinding{GPUUUID: "GPU-11111111-1111-1111-1111-111111111111", Executable: filepath.Join(dir, "adapter.sh"), JobFile: filepath.Join(dir, "job.json"), TerminalFile: filepath.Join(dir, "terminal.json")}
	gate := filepath.Join(dir, "continue")
	// The background child deliberately survives its main process. All work is
	// disposable CPU-only sleep, confined to a unique fixture unit.
	script := fmt.Sprintf("#!/bin/sh\numask 077\n/usr/bin/sleep infinity &\nwhile [ ! -f '%s' ]; do /usr/bin/sleep 0.05; done\nterminal='%s'\nprintf '{\"version\":1,\"id\":\"job-one\",\"token\":\"token-one\",\"fence\":{\"incarnation\":\"session-one\",\"epoch\":7},\"recipeSHA256\":\"recipe-one\",\"invocationId\":\"%%s\",\"outcome\":\"completed\"}' \"$INVOCATION_ID\" > \"$terminal.tmp\"\n/bin/mv -- \"$terminal.tmp\" \"$terminal\"\nexit 0\n", gate, binding.TerminalFile)
	if err := os.WriteFile(binding.Executable, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, unit)
	if err := os.WriteFile(path, []byte(gpuruntime.TrainerUnit(*binding)), 0600); err != nil {
		t.Fatal(err)
	}
	f.units = append(f.units, unit)
	command("link", path)
	command("daemon-reload")
	command("start", "--", unit)
	p := control.WorkloadProfile{Unit: unit, Trainer: binding}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	before := properties(unit)
	invocation := before["InvocationID"]
	decoded, err := hex.DecodeString(invocation)
	if err != nil || len(decoded) != 16 || before["ActiveState"] != "active" || before["SubState"] != "running" || before["ControlGroup"] == "" {
		t.Fatalf("running fixture identity: %v", before)
	}
	group := before["ControlGroup"]
	if err := os.WriteFile(gate, nil, 0600); err != nil {
		t.Fatal(err)
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		state := properties(unit)
		if state["ActiveState"] == "active" && state["SubState"] == "exited" {
			if state["InvocationID"] != invocation {
				t.Fatalf("natural exit lost invocation: %v", state)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("trainer did not reach retained active/exited state")
		case <-ticker.C:
		}
	}
	job := control.TrainingJob{ID: "job-one", Token: "token-one", Fence: control.Fence{Incarnation: "session-one", Epoch: 7}, RecipeSHA256: "recipe-one", InvocationID: invocation}
	evidence, err := f.manager.TrainingTerminal(ctx, p, job)
	if err != nil || evidence.Outcome != "completed" || !evidence.Matches(job) {
		t.Fatalf("real terminal acknowledgement: %#v, %v", evidence, err)
	}
	var stat unix.Statfs_t
	if err := unix.Statfs("/sys/fs/cgroup", &stat); err != nil || stat.Type != unix.CGROUP2_SUPER_MAGIC {
		t.Fatalf("real unified cgroup hierarchy required: %v", err)
	}
	events := filepath.Join("/sys/fs/cgroup", group, "cgroup.events")
	raw, err := os.ReadFile(events)
	if err != nil || !strings.Contains("\n"+string(raw), "\npopulated 1\n") {
		t.Fatalf("expected surviving trainer descendant, events=%q error=%v", raw, err)
	}
	command("stop", "--", unit)
	raw, err = os.ReadFile(events)
	if !os.IsNotExist(err) && (err != nil || !strings.Contains("\n"+string(raw), "\npopulated 0\n")) {
		t.Fatalf("trainer cgroup not released after stop: events=%q error=%v", raw, err)
	}
}
