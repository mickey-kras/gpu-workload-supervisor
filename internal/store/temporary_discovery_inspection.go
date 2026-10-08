package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

var ErrTemporaryDiscoveryUpgradeRequired = errors.New("temporary discovery requires the current state schema; finish normal setup activation with its quiescence check and compatible backup first")

// TemporaryDiscoveryInspection is a coherent durable read model. Eligibility
// uses the same idle, ownership, admission, and unfinished-work guards as a
// start, without authorizing runtime effects or initializing the database.
type TemporaryDiscoveryInspection struct {
	State       control.State
	Catalog     control.CatalogSnapshot
	Session     *control.TemporaryDiscoverySession
	Eligibility error
}

// InspectTemporaryDiscovery only reads the supplied database. Setup opens it
// through its trusted mode=ro path, so old live databases cannot be migrated or
// repaired by visiting discovery or requesting a temporary lifecycle operation.
func InspectTemporaryDiscovery(ctx context.Context, db *sql.DB) (TemporaryDiscoveryInspection, error) {
	result := TemporaryDiscoveryInspection{}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	var version int
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&version); err != nil {
		return result, err
	}
	if version > len(migrations) {
		return result, fmt.Errorf("database schema version %d is newer than supported version %d", version, len(migrations))
	}
	if result.State, err = readControlState(ctx, tx); err != nil {
		return result, err
	}
	if version < 12 {
		return result, ErrTemporaryDiscoveryUpgradeRequired
	}
	if result.State, err = readState(ctx, tx); err != nil {
		return result, err
	}
	if result.Catalog, err = readCatalog(ctx, tx); err != nil {
		return result, err
	}
	if result.Catalog.Revision == "" {
		return result, errors.New("temporary discovery requires an accepted workload catalog")
	}
	if version < len(migrations) {
		return result, ErrTemporaryDiscoveryUpgradeRequired
	}
	if result.Session, err = readTemporaryDiscoveryStatus(ctx, tx); err != nil {
		return result, err
	}
	state := result.State
	e := control.OperatorPrecondition{Incarnation: state.LeaseFence.Incarnation, Version: state.Version, Owner: state.Owner, ConfigurationRevision: result.Catalog.Revision}
	result.Eligibility = activationSource(ctx, tx, state, e)
	return result, nil
}
