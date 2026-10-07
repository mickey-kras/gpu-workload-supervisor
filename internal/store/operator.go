package store

import (
	"context"
	"database/sql"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

var ErrWrongOwner = errors.New("operator owner precondition failed")
var ErrConfigurationConflict = errors.New("operator configuration precondition failed")

func (s *Store) StartOperatorTransition(ctx context.Context, e control.OperatorPrecondition, tr Transition) (control.State, error) {
	return s.startTransition(ctx, e.Version, &e, tr)
}

// CheckOperatorPrecondition is the read-only fail-fast guard that keeps stale
// operator requests away from runtime effects; the writer transaction remains
// the authority and re-validates.
func (s *Store) CheckOperatorPrecondition(ctx context.Context, e control.OperatorPrecondition) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		state, err := readState(ctx, tx)
		if err != nil {
			return err
		}
		return operatorSource(ctx, tx, state, e)
	})
}
func operatorSource(ctx context.Context, tx *sql.Tx, s control.State, e control.OperatorPrecondition) error {
	if e.Incarnation == "" || e.Incarnation != s.LeaseFence.Incarnation || e.Version != s.Version {
		return ErrVersionConflict
	}
	if e.Owner != s.Owner {
		return ErrWrongOwner
	}
	var revision string
	err := tx.QueryRowContext(ctx, `SELECT revision FROM workload_catalog WHERE singleton=1`).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrConfigurationConflict
	}
	if err != nil {
		return err
	}
	if e.ConfigurationRevision == "" || revision != e.ConfigurationRevision {
		return ErrConfigurationConflict
	}
	if s.Phase != control.PhaseStable || s.Health == control.HealthError {
		return ErrUnstableState
	}
	running, err := transitionRunning(ctx, tx)
	if err != nil {
		return err
	}
	if running {
		return ErrTransitionRunning
	}
	return nil
}

func transitionRunning(ctx context.Context, tx *sql.Tx) (bool, error) {
	var running bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM transitions WHERE status='in_progress')`).Scan(&running)
	return running, err
}
