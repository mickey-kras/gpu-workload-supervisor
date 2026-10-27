package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

var ErrTemporaryCleanupRequired = errors.New("temporary discovery cleanup is required; inspect the persisted session and explicitly clean up its recorded invocation")

func temporaryDiscoveryPending(ctx context.Context, tx *sql.Tx) error {
	var pending bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM temporary_discovery_sessions WHERE status != 'completed')`).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return ErrTemporaryCleanupRequired
	}
	return nil
}

func (s *Store) TemporaryDiscoveryStatus(ctx context.Context) (*control.TemporaryDiscoverySession, error) {
	return readTemporaryDiscoveryStatus(ctx, s.db)
}

func readTemporaryDiscoveryStatus(ctx context.Context, q querier) (*control.TemporaryDiscoverySession, error) {
	var raw []byte
	var id, status string
	err := q.QueryRowContext(ctx, `SELECT id,payload,status FROM temporary_discovery_sessions WHERE status!='completed' ORDER BY rowid DESC LIMIT 1`).Scan(&id, &raw, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var v control.TemporaryDiscoverySession
	if err = decodeTemporaryRecord(raw, id, status, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func checkTemporarySession(ctx context.Context, tx *sql.Tx, state control.State, id, token string) (control.TemporaryDiscoverySession, error) {
	var raw []byte
	var session control.TemporaryDiscoverySession
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT payload,status FROM temporary_discovery_sessions WHERE id=? AND status!='completed'`, id).Scan(&raw, &status); err != nil {
		return session, err
	}
	if err := decodeTemporaryRecord(raw, id, status, &session); err != nil {
		return session, err
	}
	catalog, err := readCatalog(ctx, tx)
	if err != nil {
		return session, err
	}
	if session.ID != id || !session.PriorStopped || token == "" || session.Token != token || state.Owner != control.OwnerSupervisor || state.Admission != control.AdmissionClosed || session.Fence != state.LeaseFence || session.Version != state.Version || session.ConfigurationRevision != catalog.Revision {
		return session, ErrStaleFence
	}
	return session, nil
}

func (s *Store) CheckTemporaryDiscovery(ctx context.Context, id, token string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		state, err := readState(ctx, tx)
		if err != nil {
			return err
		}
		_, err = checkTemporarySession(ctx, tx, state, id, token)
		return err
	})
}

func (s *Store) FinishTemporaryDiscovery(ctx context.Context, id, token string) (control.State, error) {
	return s.stateTx(ctx, func(tx *sql.Tx, current control.State) (control.State, error) {
		v, err := checkTemporarySession(ctx, tx, current, id, token)
		if err != nil {
			return control.State{}, err
		}
		final := current
		final.DesiredWorkload = control.WorkloadIdle
		final.ActiveWorkload = control.WorkloadIdle
		final.Phase = control.PhaseStable
		final.Health = control.HealthHealthy
		final.Admission = control.AdmissionClosed
		next, err := s.finishTransitionTx(ctx, tx, current, id, "committed", final)
		if err != nil {
			return next, err
		}
		v.Status = "completed"
		v.Error = ""
		v.Version = next.Version
		v.UpdatedAt = s.now().UTC()
		raw, err := json.Marshal(v)
		if err != nil {
			return control.State{}, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE temporary_discovery_sessions SET payload=?,status='completed' WHERE id=?`, raw, id)
		return next, err
	})
}

func InspectTemporaryDiscoveryStatus(ctx context.Context, db *sql.DB) (*control.TemporaryDiscoverySession, error) {
	var version int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&version); err != nil {
		return nil, err
	}
	if version != len(migrations) {
		return nil, errors.New("temporary cleanup requires the current state schema; use a compatible release to clean up before upgrading")
	}
	return readTemporaryDiscoveryStatus(ctx, db)
}

func decodeTemporaryRecord(raw []byte, id, status string, session *control.TemporaryDiscoverySession) error {
	if err := json.Unmarshal(raw, session); err != nil {
		return err
	}
	if session.ID != id || session.Status != status || session.Token == "" || !session.PriorStopped {
		return errors.New("invalid persisted temporary discovery session")
	}
	if status != "starting" && status != "running" && status != "cleanup_required" {
		return errors.New("unknown persisted temporary discovery status")
	}
	return nil
}
