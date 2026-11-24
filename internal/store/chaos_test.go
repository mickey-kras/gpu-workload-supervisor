package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mickey-kras/llm-workload-supervisor/internal/control"
)

// This file injects real SIGKILL into real child processes that own a real
// SQLite store, then reopens the database and asserts the durability
// invariants the project claims: WAL + synchronous=FULL survives process
// death, committed transactions are never lost, in-flight transactions leave
// no torn records, and the transition state machine always restarts in a
// legal state that explicit recovery can settle.
//
// The child is the test binary itself re-executed with -test.run pointing at
// TestStoreChaosHarnessHelper (the os/exec documented helper-process
// pattern). The parent and child coordinate through files in a shared
// directory. Every coordination file is written to a temporary name in the
// same directory and renamed over its final name, which is atomic on POSIX,
// so a polling reader never observes a partially written file. The child
// writes "ready" (holding the committed control_state version) once it
// passes a checkpoint, "ack" (the count of fully acknowledged admission
// iterations) after each committed admit/finish pair, "txn-open" once it
// stages writes inside an explicit uncommitted transaction, and "error" if
// it fails early. The parent kills with SIGKILL after the relevant
// checkpoints appear, so the exact instruction under kill varies run to run
// while every assertion holds for every possible kill point.

const envChaosHelper = "GPU_SUPERVISOR_STORE_CHAOS_HELPER"

func TestStoreSurvivesSIGKILLDuringTransitionLoop(t *testing.T) {
	// Different checkpoints move the kill across the transition lifecycle:
	// right after the first commit, mid-loop, and deep into the loop where
	// WAL checkpointing has real history to recover.
	for _, readyAfter := range []int{1, 5, 40} {
		t.Run(fmt.Sprintf("kill_after_%d_committed_iterations", readyAfter), func(t *testing.T) {
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "state.db")
			child := startStoreChaosChild(t, dbPath, dir, "transition-loop", readyAfter, 0)
			readyVersion := waitStoreChaosReady(t, child, dir)

			killStoreChaosChild(t, child)
			assertStoreInvariantsAfterKill(t, dbPath, readyVersion, true, 0)
		})
	}
}

func TestStoreSurvivesSIGKILLDuringAdmissionLoop(t *testing.T) {
	// Admission rows are the proxy-facing records; a torn admit/finish pair
	// would be a real durability bug. The child atomically acknowledges
	// every committed pair through the "ack" checkpoint; after the kill the
	// assertion proves every acknowledged row is durably completed, which
	// readyVersion alone cannot detect because admission does not bump
	// control_state.version.
	for _, readyAfter := range []int{1, 20} {
		t.Run(fmt.Sprintf("kill_after_%d_committed_pairs", readyAfter), func(t *testing.T) {
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "state.db")
			child := startStoreChaosChild(t, dbPath, dir, "admission-loop", readyAfter, 0)
			readyVersion := waitStoreChaosReady(t, child, dir)

			killStoreChaosChild(t, child)
			assertStoreInvariantsAfterKill(t, dbPath, readyVersion, true, readStoreChaosAck(t, dir))
		})
	}
}

func TestStoreSurvivesSIGKILLWhenIdle(t *testing.T) {
	// Quiescent kill: everything committed, nothing in flight. The reopened
	// store must be bit-for-bit consistent with the checkpoint.
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	child := startStoreChaosChild(t, dbPath, dir, "idle", 3, 0)
	readyVersion := waitStoreChaosReady(t, child, dir)

	killStoreChaosChild(t, child)
	assertStoreInvariantsAfterKill(t, dbPath, readyVersion, false, 0)
}

// TestStoreSurvivesSIGKILLMidCommit is the closest honest approximation of
// power loss available to a Linux harness: the child loops through
// multi-write transactions and the parent kills it immediately after a
// checkpoint, so the dying process is reliably inside a commit window.
// synchronous=FULL plus WAL guarantees that a committed transaction survives
// and an uncommitted one rolls back atomically; fsync failure mid-commit is
// out of scope for kill -9. Generations two and three kill fresh databases
// to prove durability composes across restarts.
func TestStoreSurvivesSIGKILLMidCommit(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos harness runs three kill cycles")
	}
	for generation := 1; generation <= 3; generation++ {
		dir := t.TempDir()
		dbPath := filepath.Join(dir, "state.db")
		child := startStoreChaosChild(t, dbPath, dir, "transition-loop", generation, generation+1)
		readyVersion := waitStoreChaosReady(t, child, dir)
		killStoreChaosChild(t, child)
		assertStoreInvariantsAfterKill(t, dbPath, readyVersion, true, 0)
	}
}

// TestStoreSurvivesSIGKILLDuringOpenTransaction kills the helper while it
// holds an explicit uncommitted sql.Tx with two staged writes (a
// control_state version bump and an owner flip). Unlike MidCommit, where
// the kill may land between iterations on an idle process, the "txn-open"
// marker guarantees SIGKILL lands while the transaction is still open, so
// rollback of staged writes is exercised deterministically on every run.
func TestStoreSurvivesSIGKILLDuringOpenTransaction(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	child := startStoreChaosChild(t, dbPath, dir, "txn-hold", 1, 0)
	readyVersion := waitStoreChaosReady(t, child, dir)
	waitStoreChaosFile(t, child, dir, "txn-open")

	killStoreChaosChild(t, child)

	ctx := context.Background()
	restored, err := OpenRestored(ctx, dbPath)
	if err != nil {
		t.Fatalf("killed database failed restored-open validation: %v", err)
	}
	state, err := restored.State(ctx)
	if err != nil {
		t.Fatalf("killed database state invalid: %v", err)
	}
	_ = restored.Close()

	if state.Version != readyVersion {
		t.Fatalf("uncommitted version bump survived the kill: version %d, committed checkpoint was %d",
			state.Version, readyVersion)
	}
	if state.Owner != control.OwnerSupervisor {
		t.Fatalf("uncommitted owner flip survived the kill: owner %q, want %q", state.Owner, control.OwnerSupervisor)
	}

	assertStoreInvariantsAfterKill(t, dbPath, readyVersion, false, 0)
}

// TestStoreChaosHarnessHelper is not a test. Under envChaosHelper the test
// binary re-executes itself to run one scripted scenario against a real
// store, then blocks until the parent kills it.
func TestStoreChaosHarnessHelper(t *testing.T) {
	if os.Getenv(envChaosHelper) != "1" {
		return
	}
	dir := os.Getenv("GPU_CHAOS_DIR")
	dbPath := os.Getenv("GPU_CHAOS_DB")
	fail := func(err error) {
		_ = writeStoreChaosFile(dir, "error", []byte(err.Error()))
		os.Exit(1)
	}
	readyAfter, err := strconv.Atoi(os.Getenv("GPU_CHAOS_READY_AFTER"))
	if err != nil {
		fail(err)
	}
	iterations, err := strconv.Atoi(os.Getenv("GPU_CHAOS_ITERATIONS"))
	if err != nil {
		fail(err)
	}
	ctx := context.Background()
	s, err := Open(ctx, dbPath)
	if err != nil {
		fail(err)
	}
	defer s.Close()
	snapshot, err := s.ReplaceCatalog(ctx, "", driftCatalog(control.WorkloadText, control.WorkloadMedia))
	if err != nil {
		fail(err)
	}

	if os.Getenv("GPU_CHAOS_SCENARIO") == "txn-hold" {
		chaosHoldOpenTransaction(ctx, s, dir, fail)
	}

	settleAttempts := 0
	readyWritten := false
	for i := 0; iterations == 0 || i < iterations; i++ {
		switch os.Getenv("GPU_CHAOS_SCENARIO") {
		case "transition-loop":
			if err := chaosTransitionIteration(ctx, s, snapshot.Revision, fmt.Sprintf("chaos-%06d", i)); err != nil {
				fail(fmt.Errorf("iteration %d: %w", i, err))
			}
		case "admission-loop":
			// Settle the state into the admitting configuration first:
			// a fresh store starts reconciling with admission closed, and
			// every settle transition toggles the workload. The loop
			// converges in at most two transitions (media, then text).
			for {
				st, err := s.State(ctx)
				if err != nil {
					fail(err)
				}
				if st.Phase == control.PhaseStable && st.Owner == control.OwnerSupervisor &&
					st.Admission == control.AdmissionOpen && st.ActiveWorkload == control.WorkloadText {
					break
				}
				settleID := fmt.Sprintf("chaos-settle-%06d-%d", i, settleAttempts)
				settleAttempts++
				if err := chaosTransitionIteration(ctx, s, snapshot.Revision, settleID); err != nil {
					fail(fmt.Errorf("iteration %d settle: %w", i, err))
				}
			}
			st, err := s.State(ctx)
			if err != nil {
				fail(err)
			}
			requestID := fmt.Sprintf("chaos-req-%06d", i)
			token, err := s.AdmitWorkToken(ctx, requestID, "", control.WorkloadText, st.LeaseFence)
			if err != nil {
				fail(fmt.Errorf("iteration %d admit: %w", i, err))
			}
			if err := s.FinishWorkToken(ctx, requestID, control.WorkloadText, st.LeaseFence, token, WorkCompleted); err != nil {
				fail(fmt.Errorf("iteration %d finish: %w", i, err))
			}
			// Acknowledge the fully committed pair; the parent asserts
			// every acknowledged row is durably completed.
			if err := writeStoreChaosFile(dir, "ack", []byte(strconv.Itoa(i+1))); err != nil {
				fail(err)
			}
		case "idle":
			if err := chaosIdleBump(ctx, s); err != nil {
				fail(err)
			}
		default:
			fail(fmt.Errorf("unknown scenario %q", os.Getenv("GPU_CHAOS_SCENARIO")))
		}
		if !readyWritten && i+1 >= readyAfter {
			state, err := s.State(ctx)
			if err != nil {
				fail(err)
			}
			if err := writeStoreChaosFile(dir, "ready",
				[]byte(strconv.FormatUint(state.Version, 10))); err != nil {
				fail(err)
			}
			readyWritten = true
		}
	}
	// The scripted iterations finished before the parent killed us: park
	// forever so the kill still lands on a live process holding the store.
	select {}
}

// chaosHoldOpenTransaction stages two observable writes inside one explicit
// transaction, publishes the readiness checkpoint plus a marker proving the
// transaction is still open, and parks forever while holding the
// transaction. SIGKILL then lands with real staged-but-uncommitted writes
// that SQLite must roll back during WAL recovery.
func chaosHoldOpenTransaction(ctx context.Context, s *Store, dir string, fail func(error)) {
	pre, err := s.State(ctx)
	if err != nil {
		fail(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		fail(err)
	}
	var observedVersion uint64
	var observedOwner string
	if err := tx.QueryRowContext(ctx,
		"SELECT version, owner FROM control_state WHERE singleton = 1").Scan(&observedVersion, &observedOwner); err != nil {
		fail(err)
	}
	if observedVersion != pre.Version || observedOwner != string(pre.Owner) {
		fail(fmt.Errorf("control_state drifted between state read and transaction begin: row=(version %d owner %q) state=%+v",
			observedVersion, observedOwner, pre))
	}
	// Staged write 1: a version bump no legitimate writer ever makes; if it
	// ever survives the kill, durability is broken in the loudest way.
	if _, err := tx.ExecContext(ctx,
		"UPDATE control_state SET version = ?, updated_at = ? WHERE singleton = 1",
		observedVersion+1000000, formatTime(s.now())); err != nil {
		fail(err)
	}
	// Staged write 2: flip owner; the seed and catalog replace both leave
	// it at supervisor, so anything else after the kill proves leakage.
	flippedOwner := "user"
	if observedOwner == "user" {
		flippedOwner = "supervisor"
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE control_state SET owner = ? WHERE singleton = 1", flippedOwner); err != nil {
		fail(err)
	}
	// The committed checkpoint names the pre-transaction version; the
	// second marker proves the uncommitted transaction is still open when
	// the parent kills us.
	if err := writeStoreChaosFile(dir, "ready", []byte(strconv.FormatUint(observedVersion, 10))); err != nil {
		fail(err)
	}
	if err := writeStoreChaosFile(dir, "txn-open",
		[]byte("version="+strconv.FormatUint(observedVersion, 10)+" txn=open")); err != nil {
		fail(err)
	}
	select {}
}

// chaosTransitionIteration drives one full committed transition through the
// store, mirroring the supervisor lifecycle: draining, unloading, loading,
// verifying, committed. Every step is its own multi-write transaction.
func chaosTransitionIteration(ctx context.Context, s *Store, revision, id string) error {
	current, err := s.State(ctx)
	if err != nil {
		return err
	}
	workload := control.WorkloadMedia
	if current.DesiredWorkload == control.WorkloadMedia {
		workload = control.WorkloadText
	}
	target := current
	target.Owner = control.OwnerSupervisor
	target.DesiredWorkload = workload
	target.ActiveWorkload = workload
	target.Phase = control.PhaseStable
	target.Health = control.HealthHealthy
	target.PendingIdleDeadline = nil
	target.Admission = control.AdmissionOpen
	transition := Transition{
		ID:                    id,
		ConfigurationRevision: revision,
		Target:                target,
		Previous:              current,
		Initiator:             "chaos-helper",
		Phase:                 control.PhaseDraining,
		Deadline:              time.Now().Add(time.Minute),
	}
	next, err := s.StartTransition(ctx, current.Version, transition)
	if err != nil {
		return err
	}
	for _, phase := range []control.Phase{control.PhaseUnloading, control.PhaseLoading, control.PhaseVerifying} {
		if next, err = s.SetTransitionPhase(ctx, transition.ID, next.Version, phase); err != nil {
			return err
		}
	}
	final := target
	final.LeaseFence = next.LeaseFence
	_, err = s.FinishTransition(ctx, transition.ID, "committed", next.Version, final)
	return err
}

func chaosIdleBump(ctx context.Context, s *Store) error {
	state, err := s.State(ctx)
	if err != nil {
		return err
	}
	_, err = s.UpdateState(ctx, state.Version, state)
	return err
}

// writeStoreChaosFile writes a chaos coordination file atomically: write a
// temporary file in the same directory, then rename it over the target.
// Rename within one directory is atomic on POSIX, so a polling parent never
// reads a partially written or empty checkpoint.
func writeStoreChaosFile(dir, name string, data []byte) error {
	tmp := filepath.Join(dir, name+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name))
}

type storeChaosChild struct {
	cmd      *exec.Cmd
	waitDone chan error
}

func startStoreChaosChild(t *testing.T, dbPath, dir, scenario string, readyAfter, iterations int) *storeChaosChild {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestStoreChaosHarnessHelper$", "-test.v")
	cmd.Env = append(os.Environ(),
		envChaosHelper+"=1",
		"GPU_CHAOS_DB="+dbPath,
		"GPU_CHAOS_DIR="+dir,
		"GPU_CHAOS_SCENARIO="+scenario,
		"GPU_CHAOS_READY_AFTER="+strconv.Itoa(readyAfter),
		"GPU_CHAOS_ITERATIONS="+strconv.Itoa(iterations),
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	child := &storeChaosChild{cmd: cmd, waitDone: make(chan error, 1)}
	go func() { child.waitDone <- cmd.Wait() }()
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	return child
}

// waitStoreChaosReady polls for the child's checkpoint file. It fails if the
// child dies first or nothing happens within the generous CI timeout. A
// successfully read but unparseable checkpoint keeps polling briefly: the
// atomic rename makes a partial read impossible, but staying defensive costs
// nothing.
func waitStoreChaosReady(t *testing.T, child *storeChaosChild, dir string) uint64 {
	t.Helper()
	ready := filepath.Join(dir, "ready")
	failure := filepath.Join(dir, "error")
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-child.waitDone:
			t.Fatalf("chaos helper exited before checkpoint: %v", err)
		default:
		}
		if data, err := os.ReadFile(ready); err == nil {
			if version, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64); err == nil {
				return version
			}
		}
		if data, err := os.ReadFile(failure); err == nil {
			t.Fatalf("chaos helper failed: %s", data)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for chaos helper checkpoint")
	return 0
}

// waitStoreChaosFile polls until the named coordination file exists. It
// fails if the child dies first or the deadline expires.
func waitStoreChaosFile(t *testing.T, child *storeChaosChild, dir, name string) {
	t.Helper()
	path := filepath.Join(dir, name)
	failure := filepath.Join(dir, "error")
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-child.waitDone:
			t.Fatalf("chaos helper exited before writing %s: %v", name, err)
		default:
		}
		if _, err := os.ReadFile(path); err == nil {
			return
		}
		if data, err := os.ReadFile(failure); err == nil {
			t.Fatalf("chaos helper failed: %s", data)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for chaos helper file %s", name)
}

// readStoreChaosAck reads the child's admission acknowledgment checkpoint
// after the kill; absent means no admit/finish pair fully committed before
// the kill, and the registered_work assertion is skipped.
func readStoreChaosAck(t *testing.T, dir string) uint64 {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "ack"))
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	ack, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		t.Fatalf("unparseable admission ack checkpoint %q: %v", data, err)
	}
	return ack
}

func killStoreChaosChild(t *testing.T, child *storeChaosChild) {
	t.Helper()
	if child.cmd.Process == nil {
		t.Fatal("chaos helper has no process")
	}
	if err := child.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL chaos helper: %v", err)
	}
	select {
	case err := <-child.waitDone:
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("chaos helper wait error = %v", err)
		}
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && !status.Signaled() {
			t.Fatalf("chaos helper exited on its own with status %v", status)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("chaos helper did not die after SIGKILL")
	}
}

// assertStoreInvariantsAfterKill reopens a killed database the same way the
// restored-state path does and checks every invariant that must hold after
// process death, at any injection point. When transitions may be in flight,
// it also runs the same explicit recovery the supervisor runs and proves the
// database accepts and completes it. admissionAck is the child's
// acknowledgment count from the admission scenario: when positive, every
// acknowledged iteration must have a durably completed registered_work row,
// because admissions never bump control_state.version and would otherwise
// be invisible to the version checkpoint.
func assertStoreInvariantsAfterKill(t *testing.T, dbPath string, readyVersion uint64, expectTransition bool, admissionAck uint64) {
	t.Helper()
	ctx := context.Background()

	// OpenRestored validates the restored database: quick_check, initialized
	// schema, readable control state. A torn commit or corrupt WAL fails here.
	restored, err := OpenRestored(ctx, dbPath)
	if err != nil {
		t.Fatalf("killed database failed restored-open validation: %v", err)
	}
	defer restored.Close()

	state, err := restored.State(ctx)
	if err != nil {
		t.Fatalf("killed database state invalid: %v", err)
	}
	if state.Version < readyVersion {
		t.Fatalf("committed state lost: version %d, checkpoint was %d", state.Version, readyVersion)
	}
	if err := state.Validate(); err != nil {
		t.Fatalf("killed database state violates invariants: %v", err)
	}

	// Acknowledged admissions are never torn: the child writes the ack
	// checkpoint only after the admit and finish both commit, so every
	// iteration below admissionAck must exist with completed_at set.
	// Iterations past the ack may be interrupted by the kill and are not
	// checked.
	for j := uint64(0); j < admissionAck; j++ {
		var completedAt sql.NullString
		err := restored.db.QueryRowContext(ctx,
			`SELECT completed_at FROM registered_work WHERE request_id = ?`,
			fmt.Sprintf("chaos-req-%06d", j)).Scan(&completedAt)
		if errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("acknowledged admission %d lost: no registered_work row", j)
		}
		if err != nil {
			t.Fatal(err)
		}
		if !completedAt.Valid {
			t.Fatalf("acknowledged admission %d incomplete: completed_at NULL", j)
		}
	}

	var inProgress int
	if err := restored.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM transitions WHERE status = 'in_progress'`).Scan(&inProgress); err != nil {
		t.Fatal(err)
	}
	if inProgress > 1 {
		t.Fatalf("killed database holds %d in-progress transitions, want at most 1", inProgress)
	}
	running, err := restored.InProgressTransition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if running != "" && state.Admission != control.AdmissionClosed {
		t.Fatalf("in-progress transition with admission %q", state.Admission)
	}
	if running != "" {
		switch state.Phase {
		case control.PhaseDraining, control.PhaseUnloading, control.PhaseLoading, control.PhaseVerifying:
		default:
			t.Fatalf("in-progress transition in illegal phase %q", state.Phase)
		}
	}
	if expectTransition && running == "" && state.Phase != control.PhaseStable {
		t.Fatalf("no in-progress transition but phase %q", state.Phase)
	}

	var integrity string
	if err := restored.db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		t.Fatalf("integrity_check on killed database: %v", err)
	}
	if integrity != "ok" {
		t.Fatalf("integrity_check on killed database: %s", integrity)
	}

	// Committed transitions are never ambiguous: every row the writer
	// finished is 'committed', the one interrupted by the kill (if any) is
	// still 'in_progress', and nothing the writer never failed is 'failed'.
	var committed, failed int
	if err := restored.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM transitions WHERE status = 'committed'`).Scan(&committed); err != nil {
		t.Fatal(err)
	}
	if err := restored.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM transitions WHERE status = 'failed'`).Scan(&failed); err != nil {
		t.Fatal(err)
	}
	if failed != 0 {
		t.Fatalf("killed database holds %d failed transitions the writer never wrote", failed)
	}
	if expectTransition && committed == 0 {
		t.Fatal("killed database lost every committed transition")
	}

	// The recovery the supervisor runs after a crash must complete on the
	// killed database. This is the durability claim in operational terms.
	final := state
	final.Phase = control.PhaseReconciling
	final.Health = control.HealthError
	final.Admission = control.AdmissionClosed
	final.ActiveWorkload = control.WorkloadUnknown
	final.PendingIdleDeadline = nil
	recovered, err := restored.Recover(ctx, state.Version, final, "chaos-recovery")
	if err != nil {
		t.Fatalf("explicit recovery rejected killed database: %v", err)
	}
	if err := recovered.Validate(); err != nil {
		t.Fatalf("recovered state invalid: %v", err)
	}
	if runningAfter, err := restored.InProgressTransition(ctx); err != nil || runningAfter != "" {
		t.Fatalf("recovery left transition in progress %q: %v", runningAfter, err)
	}
}
