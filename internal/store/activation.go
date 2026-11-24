package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/mickey-kras/llm-workload-supervisor/internal/control"
)

var (
	// ErrNotIdle reports that the supervisor is neither in an idle,
	// admission-closed state nor in a stable workload it may hand off, so
	// activation must be deferred.
	ErrNotIdle = errors.New("workload activation requires idle supervisor state")
	// ErrUnresolvedWork reports unfinished admitted work blocking activation;
	// the activation may be retried once the work drains.
	ErrUnresolvedWork = errors.New("unfinished admitted work blocks workload activation")
)

// CheckActivationPrecondition is the read-only fail-fast guard; the writer
// transaction remains the authority and re-validates everything.
func (s *Store) CheckActivationPrecondition(ctx context.Context, e control.OperatorPrecondition, target control.Workload) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		state, err := readState(ctx, tx)
		if err != nil {
			return err
		}
		return activationSource(ctx, tx, state, e, target)
	})
}

// StartActivationTransition commits the activation drain inside one writer
// transaction with the full operator and activation preconditions revalidated.
func (s *Store) StartActivationTransition(ctx context.Context, e control.OperatorPrecondition, tr Transition) (control.State, error) {
	if tr.ID == "" {
		return control.State{}, errors.New("transition id is empty")
	}
	// No version pre-check here: activationSource reports a running transition
	// as busy before the stale-version conflict, because a live transition
	// bumps the version and persists a non-stable phase.
	return s.stateTx(ctx, func(tx *sql.Tx, current control.State) (control.State, error) {
		if err := activationSource(ctx, tx, current, e, tr.Target.ActiveWorkload); err != nil {
			return control.State{}, err
		}
		if err := validateTransitionCatalog(ctx, tx, tr); err != nil {
			return control.State{}, err
		}
		return s.beginTransitionTx(ctx, tx, current, tr)
	})
}

// activationSource reports a running transition as busy before the stability
// precondition, because a live transition persists a non-stable phase which
// would otherwise shadow the transition check. The source is Idle, or another
// stable workload with open admission. A switch hands off only when no
// admitted work is open, so it never cuts off work in flight.
func activationSource(ctx context.Context, tx *sql.Tx, state control.State, e control.OperatorPrecondition, target control.Workload) error {
	running, err := transitionRunning(ctx, tx)
	if err != nil {
		return err
	}
	if running {
		return ErrTransitionRunning
	}
	if err := operatorSource(ctx, tx, state, e); err != nil {
		return err
	}
	if state.Owner != control.OwnerSupervisor {
		return ErrWrongOwner
	}
	if state.Health != control.HealthHealthy {
		return ErrUnstableState
	}
	idle := state.Admission == control.AdmissionClosed && state.ActiveWorkload == control.WorkloadIdle && state.DesiredWorkload == control.WorkloadIdle
	switchable := state.Admission == control.AdmissionOpen && state.ActiveWorkload == state.DesiredWorkload &&
		state.ActiveWorkload != control.WorkloadIdle && state.ActiveWorkload != control.WorkloadUnknown && state.ActiveWorkload != target
	if !idle && !switchable {
		return ErrNotIdle
	}
	pending, err := pendingWorkTx(ctx, tx)
	if err != nil {
		return err
	}
	if pending > 0 {
		return ErrUnresolvedWork
	}
	return nil
}
