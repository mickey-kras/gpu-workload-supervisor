package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/mickey-kras/llm-workload-supervisor/internal/control"
	"github.com/mickey-kras/llm-workload-supervisor/internal/store"
)

// This file proves the supervisor's crash-safety claim under real process
// death instead of unit fixtures: a child process drives Controller.Switch
// into the loading phase, where the runtime start effect is in flight, and
// the parent kills it with SIGKILL. The parent then reopens the store and
// drives the real recovery path with a fresh controller, asserting the
// database restarts in the exact legal state the kill left behind and that
// explicit recovery converges to a usable system.
//
// The child is this test binary re-executed (the os/exec helper-process
// pattern), coordinated through files like the store chaos harness.

const envCrashHelper = "GPU_SUPERVISOR_CRASH_HELPER"

// blockingStartRuntime models the dangerous window: the runtime start effect
// has been invoked but never returned, exactly where a crash strands a
// transition. It writes the ready checkpoint at Start entry, then holds the
// call so the kill lands on a live effect.
type blockingStartRuntime struct {
	fakeRuntime
	onStart func()
}

func (r *blockingStartRuntime) Start(ctx context.Context, workload control.Workload) error {
	if r.onStart != nil {
		r.onStart()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(10 * time.Minute): // safety net for a lost parent kill
		return errors.New("blockingStartRuntime outlived the chaos harness")
	}
}

func TestSupervisorSurvivesSIGKILLMidTransition(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	child := startCrashChild(t, dbPath, dir)
	waitCrashReady(t, child, dir)
	killCrashChild(t, child)

	ctx := context.Background()
	// The store itself must pass restored-state validation after the kill.
	stateStore, err := store.OpenRestored(ctx, dbPath)
	if err != nil {
		t.Fatalf("killed supervisor database failed restored-open validation: %v", err)
	}
	defer stateStore.Close()

	// The kill landed while the start effect was in flight. Between the
	// committed phase advance to loading and the runtime call, the
	// controller only appends transition events, so the resumed state must
	// be exact, not merely "a legal phase": drift here means the transition
	// journal and the control state disagree about where the process died.
	state, err := stateStore.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.Phase != control.PhaseLoading {
		t.Fatalf("killed mid-start transition resumed in phase %q, want loading", state.Phase)
	}
	if state.Admission != control.AdmissionClosed {
		t.Fatalf("killed mid-start transition left admission %q", state.Admission)
	}
	crashedID, err := stateStore.InProgressTransition(ctx)
	if err != nil || crashedID == "" {
		t.Fatalf("killed mid-start transition left no in-progress transition: %q %v", crashedID, err)
	}

	// A fresh controller, with no memory of the dead process, must recover
	// through the same explicit path the operator runs. Nothing was ever
	// started, so recovery converges on idle and journals the crashed
	// transition as failed.
	controller := testController(t, stateStore, &fakeRuntime{})
	recovered, err := controller.Recover(ctx)
	if err != nil {
		t.Fatalf("fresh controller rejected killed database: %v", err)
	}
	if err := recovered.Validate(); err != nil {
		t.Fatalf("recovered state invalid: %v", err)
	}
	if recovered.Phase != control.PhaseStable || recovered.ActiveWorkload != control.WorkloadIdle ||
		recovered.DesiredWorkload != control.WorkloadIdle {
		t.Fatalf("recovery converged to %s/%s in phase %q, want settled idle",
			recovered.DesiredWorkload, recovered.ActiveWorkload, recovered.Phase)
	}
	if running, err := stateStore.InProgressTransition(ctx); err != nil || running != "" {
		t.Fatalf("recovery left transition in progress %q: %v", running, err)
	}

	// The system must be usable afterwards: a new switch from the recovered
	// state completes end to end. The journal-level proof that the crashed
	// transition row was marked failed lives in the store chaos harness,
	// which queries the transitions table directly.
	final, err := controller.Switch(ctx, control.WorkloadText, "post-crash")
	if err != nil {
		t.Fatalf("switch after crash recovery: %v", err)
	}
	if final.Phase != control.PhaseStable || final.ActiveWorkload != control.WorkloadText ||
		final.Admission != control.AdmissionOpen {
		t.Fatalf("post-recovery switch settled at %s/%s admission %q",
			final.ActiveWorkload, final.Phase, final.Admission)
	}
}

// TestCrashHarnessHelper is not a test. Under envCrashHelper the test binary
// re-executes itself: it settles a fresh store, starts a switch whose
// runtime start blocks forever, and waits for the parent's SIGKILL.
func TestCrashHarnessHelper(t *testing.T) {
	if os.Getenv(envCrashHelper) != "1" {
		return
	}
	dir := os.Getenv("GPU_CRASH_DIR")
	dbPath := os.Getenv("GPU_CRASH_DB")
	fail := func(err error) {
		_ = os.WriteFile(filepath.Join(dir, "error"), []byte(err.Error()), 0o600)
		os.Exit(1)
	}
	ctx := context.Background()
	stateStore, err := store.Open(ctx, dbPath)
	if err != nil {
		fail(err)
	}
	defer stateStore.Close()
	snapshot, err := stateStore.ReplaceCatalog(ctx, "", testCatalog())
	if err != nil {
		fail(err)
	}
	runtime := &blockingStartRuntime{onStart: func() {
		if err := os.WriteFile(filepath.Join(dir, "ready"), []byte("started"), 0o600); err != nil {
			fail(err)
		}
	}}
	// ActionTimeout bounds a runtime effect; it must far outlast the
	// parent's kill so SIGKILL, not the timeout, ends the blocked start.
	controller, err := newController(stateStore, runtime, Config{
		Catalog:      &snapshot,
		DrainTimeout: time.Second, VerifyTimeout: time.Second,
		ActionTimeout:  5 * time.Minute,
		CleanupTimeout: time.Second, FinalizeTimeout: time.Second,
		PollInterval: time.Millisecond,
	}, time.Now, func() (string, error) {
		// Distinct from testController's fixed id: transition_id is unique
		// across every row ever written, including failed ones, so the
		// crashed transition must not collide with the post-recovery switch.
		return "99999999-9999-4999-8999-999999999999", nil
	})
	if err != nil {
		fail(err)
	}
	if _, err := controller.Reconcile(ctx); err != nil {
		fail(fmt.Errorf("settle: %w", err))
	}
	if _, err := controller.Switch(ctx, control.WorkloadMedia, "chaos"); err != nil {
		fail(fmt.Errorf("switch: %w", err))
	}
	fail(errors.New("switch returned before the parent killed the helper"))
}

type crashChild struct {
	cmd      *exec.Cmd
	waitDone chan error
}

func startCrashChild(t *testing.T, dbPath, dir string) *crashChild {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashHarnessHelper$", "-test.v")
	cmd.Env = append(os.Environ(),
		envCrashHelper+"=1",
		"GPU_CRASH_DB="+dbPath,
		"GPU_CRASH_DIR="+dir,
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	child := &crashChild{cmd: cmd, waitDone: make(chan error, 1)}
	go func() { child.waitDone <- cmd.Wait() }()
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	return child
}

func waitCrashReady(t *testing.T, child *crashChild, dir string) {
	t.Helper()
	ready := filepath.Join(dir, "ready")
	failure := filepath.Join(dir, "error")
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-child.waitDone:
			t.Fatalf("crash helper exited before checkpoint: %v", err)
		default:
		}
		if _, err := os.Stat(ready); err == nil {
			return
		}
		if data, err := os.ReadFile(failure); err == nil {
			t.Fatalf("crash helper failed: %s", data)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for crash helper checkpoint")
}

func killCrashChild(t *testing.T, child *crashChild) {
	t.Helper()
	if err := child.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL crash helper: %v", err)
	}
	select {
	case err := <-child.waitDone:
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("crash helper wait error = %v", err)
		}
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && !status.Signaled() {
			t.Fatalf("crash helper exited on its own with status %v", status)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("crash helper did not die after SIGKILL")
	}
}
