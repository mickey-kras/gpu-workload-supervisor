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
	err := q.QueryRowContext(ctx, `SELECT payload FROM temporary_discovery_sessions ORDER BY rowid DESC LIMIT 1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var v control.TemporaryDiscoverySession
	if err = json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func (s *Store) StartTemporaryDiscovery(ctx context.Context, e control.OperatorPrecondition, tr Transition, session control.TemporaryDiscoverySession) (control.State, error) {
	return s.stateTx(ctx, func(tx *sql.Tx, current control.State) (control.State, error) {
		if err := activationSource(ctx, tx, current, e); err != nil {
			return control.State{}, err
		}
		if err := temporaryDiscoveryPending(ctx, tx); err != nil {
			return control.State{}, err
		}
		if session.ID != tr.ID || session.Token == "" || session.Status != "starting" || !session.PriorStopped || session.InvocationID != "" || tr.Target.DesiredWorkload != control.WorkloadIdle || tr.ConfigurationRevision != e.ConfigurationRevision {
			return control.State{}, errors.New("invalid temporary discovery entry")
		}
		if err := validateTransitionCatalog(ctx, tx, tr); err != nil {
			return control.State{}, err
		}
		next, err := s.beginTransitionTx(ctx, tx, current, tr)
		if err != nil {
			return next, err
		}
		session.Fence = next.LeaseFence
		session.Version = next.Version
		session.ConfigurationRevision = e.ConfigurationRevision
		session.UpdatedAt = s.now().UTC()
		raw, err := json.Marshal(session)
		if err != nil {
			return control.State{}, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO temporary_discovery_sessions(id,payload,status) VALUES(?,?,?)`, session.ID, raw, session.Status)
		return next, err
	})
}

func checkTemporarySession(ctx context.Context, tx *sql.Tx, state control.State, id, token string) (control.TemporaryDiscoverySession, error) {
	var raw []byte
	var session control.TemporaryDiscoverySession
	if err := tx.QueryRowContext(ctx, `SELECT payload FROM temporary_discovery_sessions WHERE id=? AND status!='completed'`, id).Scan(&raw); err != nil {
		return session, err
	}
	if err := json.Unmarshal(raw, &session); err != nil {
		return session, err
	}
	catalog, err := readCatalog(ctx, tx)
	if err != nil {
		return session, err
	}
	if token == "" || session.Token != token || state.Owner != control.OwnerSupervisor || state.Admission != control.AdmissionClosed || session.Fence != state.LeaseFence || session.Version != state.Version || session.ConfigurationRevision != catalog.Revision {
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

// UpdateTemporaryDiscovery changes evidence only while the exact session owns
// the durable closed control generation. Invocation identity cannot be replaced.
func (s *Store) UpdateTemporaryDiscovery(ctx context.Context, id, token, invocation, status, message string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		current, err := readState(ctx, tx)
		if err != nil {
			return err
		}
		v, err := checkTemporarySession(ctx, tx, current, id, token)
		if err != nil {
			return err
		}
		if err := validateTemporaryUpdate(v, invocation, status); err != nil {
			return err
		}
		v.InvocationID = invocation
		v.Status = status
		v.Error = message
		v.UpdatedAt = s.now().UTC()
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE temporary_discovery_sessions SET payload=?,status=? WHERE id=?`, raw, status, id)
		return err
	})
}

func validateTemporaryUpdate(v control.TemporaryDiscoverySession, invocation, status string) error {
	if status != "running" && status != "cleanup_required" {
		return errors.New("invalid temporary discovery status")
	}
	if invocation != "" && (v.LaunchEvidence.InvocationID != invocation || v.LaunchEvidence.JobID == "" || v.LaunchEvidence.ActivationTimestamp == "") {
		return ErrStaleFence
	}
	if v.InvocationID != "" && invocation != v.InvocationID {
		return ErrStaleFence
	}
	return nil
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

func (s *Store) RecordTemporaryLaunch(ctx context.Context, id, token string, e control.TemporaryDiscoveryLaunchEvidence) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		current, err := readState(ctx, tx)
		if err != nil {
			return err
		}
		v, err := checkTemporarySession(ctx, tx, current, id, token)
		if err != nil {
			return err
		}
		if v.LaunchEvidence.JobID != "" {
			return ErrStaleFence
		}
		v.LaunchEvidence = e
		v.UpdatedAt = s.now().UTC()
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE temporary_discovery_sessions SET payload=? WHERE id=?`, raw, id)
		return err
	})
}
