package store

import (
	"context"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

// This file is a seeded property test over the store's transition state
// machine: random *legal* operation sequences against a real SQLite store,
// asserting after every single step that the persisted control state still
// validates, that version and fence epoch never move backwards, that at most
// one transition is in progress, and that the database always accepts the
// explicit recovery a real crash would require. The seeds are fixed, so CI
// runs the exact same sequences every time; the chaos harness in
// chaos_test.go covers the same invariants under real process death.

type propertyOp int

const (
	opStartTransition propertyOp = iota
	opAdvancePhase
	opFinishCommitted
	opFinishFailed
	opRecover
	opRotateFence
	opRotateIncarnation
	opAdmitWork
	opFinishWork
)

func TestRandomTransitionSequencesPreserveInvariants(t *testing.T) {
	ctx := context.Background()

	for trial := 0; trial < 5; trial++ {
		t.Run(fmt.Sprintf("trial_%d", trial), func(t *testing.T) {
			s := propertyStore(t)
			snapshot, err := s.ReplaceCatalog(ctx, "", driftCatalog(control.WorkloadText, control.WorkloadMedia))
			if err != nil {
				t.Fatal(err)
			}
			rng := rand.New(rand.NewPCG(uint64(trial)+1, 0x9e3779b97f4a7c15))

			var (
				inProgress   bool
				transitionID string
				pendingReq   string
				pendingToken string
				pendingFence control.Fence
			)
			const steps = 250

			for step := 0; step < steps; step++ {
				state, err := s.State(ctx)
				if err != nil {
					t.Fatalf("step %d: read state: %v", step, err)
				}
				if err := state.Validate(); err != nil {
					t.Fatalf("step %d: persisted state violates invariants: %v", step, err)
				}

				op := choosePropertyOp(rng, state, inProgress, pendingToken != "")
				switch op {
				case opStartTransition:
					workload := control.WorkloadMedia
					if state.DesiredWorkload == control.WorkloadMedia {
						workload = control.WorkloadText
					}
					transitionID = fmt.Sprintf("prop-%d-%d", trial, step)
					_, err = s.StartTransition(ctx, state.Version, Transition{
						ID:                    transitionID,
						ConfigurationRevision: snapshot.Revision,
						Target:                propertyTarget(state, workload),
						Previous:              state,
						Initiator:             "property-test",
						Phase:                 control.PhaseDraining,
						Deadline:              time.Now().Add(time.Minute),
					})
					if err == nil {
						inProgress = true
					}
				case opAdvancePhase:
					// While a transition runs, the persisted state phase is the
					// transition phase, so the step ordering comes straight from
					// the state row.
					nextPhase := map[control.Phase]control.Phase{
						control.PhaseDraining:  control.PhaseUnloading,
						control.PhaseUnloading: control.PhaseLoading,
						control.PhaseLoading:   control.PhaseVerifying,
					}[state.Phase]
					if nextPhase == "" {
						t.Fatalf("step %d: no ordered phase after %q", step, state.Phase)
					}
					_, err = s.SetTransitionPhase(ctx, transitionID, state.Version, nextPhase)
				case opFinishCommitted:
					_, err = s.FinishTransition(ctx, transitionID, "committed", state.Version,
						propertyFinish(state))
					if err == nil {
						inProgress = false
					}
				case opFinishFailed:
					_, err = s.FinishTransition(ctx, transitionID, "failed", state.Version,
						propertyFailed(state))
					if err == nil {
						inProgress = false
					}
				case opRecover:
					_, err = s.Recover(ctx, state.Version, propertyFailed(state), "property-recovery")
					if err == nil {
						inProgress = false
					}
				case opRotateFence:
					// The supervisor only rotates the fence while
					// transitioning, settled on idle, or latched in error:
					// a stable non-idle state must keep admission open.
					_, err = s.RotateFenceAndCloseAdmission(ctx, state.Version)
				case opRotateIncarnation:
					_, err = s.RotateIncarnation(ctx)
					if err == nil && inProgress {
						// RotateIncarnation fails in-progress transitions:
						// it models a state restore, exactly like the real
						// restored-state path.
						inProgress = false
					}
					if err == nil {
						// The restore path abandons every admitted request.
						pendingReq = ""
						pendingToken = ""
						pendingFence = control.Fence{}
					}
				case opAdmitWork:
					pendingReq = fmt.Sprintf("prop-req-%d-%d", trial, step)
					pendingFence = state.LeaseFence
					pendingToken, err = s.AdmitWorkToken(ctx, pendingReq, "", control.WorkloadText, state.LeaseFence)
					if err != nil {
						pendingReq = ""
						pendingFence = control.Fence{}
					}
				case opFinishWork:
					outcome := WorkCompleted
					if rng.IntN(2) == 0 {
						outcome = WorkAbandoned
					}
					// Completion authority binds to the fence presented at
					// admission: a fence rotation (transition start, operator
					// rotation, recovery) must not strand admitted work.
					err = s.FinishWorkToken(ctx, pendingReq, control.WorkloadText, pendingFence, pendingToken, outcome)
					if err == nil {
						pendingReq = ""
						pendingToken = ""
						pendingFence = control.Fence{}
					}
				}
				if err != nil {
					t.Fatalf("step %d: legal op %d rejected: %v", step, op, err)
				}

				// Invariants after every accepted operation.
				after, err := s.State(ctx)
				if err != nil {
					t.Fatalf("step %d: read state after op %d: %v", step, op, err)
				}
				if err := after.Validate(); err != nil {
					t.Fatalf("step %d: op %d left invalid state: %v", step, op, err)
				}
				if after.Version < state.Version {
					t.Fatalf("step %d: op %d moved version backwards %d -> %d", step, op, state.Version, after.Version)
				}
				if after.LeaseFence.Incarnation == state.LeaseFence.Incarnation &&
					after.LeaseFence.Epoch < state.LeaseFence.Epoch {
					// The epoch counts fence rotations within one incarnation; a
					// new incarnation supersedes the old one wholesale.
					t.Fatalf("step %d: op %d moved fence epoch backwards", step, op)
				}
				var running int
				if err := s.db.QueryRowContext(ctx,
					`SELECT COUNT(*) FROM transitions WHERE status = 'in_progress'`).Scan(&running); err != nil {
					t.Fatal(err)
				}
				if running > 1 {
					t.Fatalf("step %d: op %d left %d transitions in progress", step, op, running)
				}
				if (running == 1) != inProgress {
					t.Fatalf("step %d: op %d tracking drift: db=%d tracked=%v", step, op, running, inProgress)
				}
			}

			// A crash at the end of the sequence must be recoverable through
			// the same explicit recovery the supervisor runs.
			final, err := s.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if inProgress {
				if _, err := s.Recover(ctx, final.Version, propertyFailed(final), "property-final"); err != nil {
					t.Fatalf("final recovery: %v", err)
				}
			}
			settled, err := s.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := settled.Validate(); err != nil {
				t.Fatalf("settled state invalid: %v", err)
			}
			if running, err := s.InProgressTransition(ctx); err != nil || running != "" {
				t.Fatalf("settled store still transitioning: %q %v", running, err)
			}
			var integrity string
			if err := s.db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
				t.Fatalf("integrity_check = %q, %v", integrity, err)
			}
		})
	}
}

func propertyTarget(state control.State, workload control.Workload) control.State {
	target := state
	target.Owner = control.OwnerSupervisor
	target.DesiredWorkload = workload
	target.ActiveWorkload = workload
	target.Phase = control.PhaseStable
	target.Health = control.HealthHealthy
	target.PendingIdleDeadline = nil
	target.Admission = control.AdmissionOpen
	return target
}

// propertyFinish is the committed final state: the current fence, because
// SetTransitionPhase already rotated it, and a stable open target.
func propertyFinish(state control.State) control.State {
	final := propertyTarget(state, state.DesiredWorkload)
	final.LeaseFence = state.LeaseFence
	return final
}

// propertyFailed is the latched recovery state the supervisor writes when a
// transition fails or a crash forces explicit recovery.
func propertyFailed(state control.State) control.State {
	final := state
	final.Phase = control.PhaseReconciling
	final.Health = control.HealthError
	final.Admission = control.AdmissionClosed
	final.ActiveWorkload = control.WorkloadUnknown
	final.PendingIdleDeadline = nil
	final.LeaseFence = state.LeaseFence
	return final
}

func choosePropertyOp(rng *rand.Rand, state control.State, inProgress bool, pendingWork bool) propertyOp {
	var ops []propertyOp
	add := func(op propertyOp, weight int) {
		for i := 0; i < weight; i++ {
			ops = append(ops, op)
		}
	}
	if inProgress {
		if state.Phase != control.PhaseVerifying {
			add(opAdvancePhase, 3)
		}
		add(opFinishFailed, 1)
		add(opRecover, 1)
		add(opRotateIncarnation, 1)
		if state.Phase == control.PhaseVerifying {
			add(opFinishCommitted, 3)
		}
	} else {
		add(opStartTransition, 4)
		add(opRotateIncarnation, 1)
		if state.Phase != control.PhaseStable || state.DesiredWorkload == control.WorkloadIdle || state.Health == control.HealthError {
			add(opRotateFence, 1)
		}
		if state.Phase == control.PhaseStable && state.Owner == control.OwnerSupervisor &&
			state.Admission == control.AdmissionOpen && state.ActiveWorkload == control.WorkloadText && !pendingWork {
			add(opAdmitWork, 3)
		}
	}
	if pendingWork {
		add(opFinishWork, 4)
	}
	if len(ops) == 0 {
		add(opRotateIncarnation, 1)
	}
	return ops[rng.IntN(len(ops))]
}

// propertyStore returns a store whose lease incarnation rotates on every
// call, unlike testStore's two-value fixture: RotateIncarnation requires a
// fresh incarnation each time.
func propertyStore(t *testing.T) *Store {
	t.Helper()
	var counter uint64
	uuid := func() (string, error) {
		counter++
		return fmt.Sprintf("00000000-0000-4000-8000-%012d", counter), nil
	}
	s, err := open(context.Background(), filepath.Join(t.TempDir(), "state.db"), fixedClock(), uuid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
