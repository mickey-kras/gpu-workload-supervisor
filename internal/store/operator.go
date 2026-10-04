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
	return s.startTransition(ctx, e.Version, nil, &e, tr)
}
func operatorSource(ctx context.Context, tx *sql.Tx, s control.State, e control.OperatorPrecondition) error {
	if e.Incarnation == "" || e.Incarnation != s.LeaseFence.Incarnation || e.Version != s.Version {
		return ErrVersionConflict
	}
	if e.Owner != s.Owner {
		return ErrWrongOwner
	}
	var revision string
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM workload_catalog WHERE singleton=1`).Scan(&revision); err != nil {
		return err
	}
	if e.ConfigurationRevision == "" || revision != e.ConfigurationRevision {
		return ErrConfigurationConflict
	}
	if s.Phase != control.PhaseStable || s.Health == control.HealthError {
		return ErrRecoveryRequired
	}
	var running bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM transitions WHERE status='in_progress')`).Scan(&running); err != nil {
		return err
	}
	if running {
		return ErrTransitionRunning
	}
	return nil
}
