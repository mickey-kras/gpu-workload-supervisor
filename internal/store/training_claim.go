package store

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

// ClaimTraining reserves only request identity, before any GPU handoff. It is
// durable and single-use; it is neither registered GPU work nor a dispatch queue.
func (s *Store) ClaimTraining(ctx context.Context, id string, workload control.Workload, revision string) (control.TrainingJob, error) {
	var claim control.TrainingJob
	if err := s.PreflightTraining(ctx, id, workload, revision); err != nil {
		return claim, err
	}
	token, err := s.uuid()
	if err != nil {
		return claim, err
	}
	claim = control.TrainingJob{ID: id, Token: token, Workload: workload, CatalogRevision: revision, Status: "preparing", UpdatedAt: s.now().UTC()}
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		if err := checkTrainingRequestID(ctx, tx, id); err != nil {
			return err
		}
		catalog, err := readCatalog(ctx, tx)
		if err != nil {
			return err
		}
		if catalog.Revision != revision {
			return ErrVersionConflict
		}
		raw, err := json.Marshal(claim)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO training_claims(id,payload,status) VALUES(?,?,'preparing')`, id, raw)
		return err
	})
	return claim, err
}

func readTrainingClaim(ctx context.Context, q querier, id string) (control.TrainingJob, string, error) {
	var claim control.TrainingJob
	var status string
	var raw []byte
	if err := q.QueryRowContext(ctx, `SELECT payload,status FROM training_claims WHERE id=?`, id).Scan(&raw, &status); err != nil {
		return claim, status, err
	}
	err := json.Unmarshal(raw, &claim)
	return claim, status, err
}

func checkTrainingClaim(ctx context.Context, tx *sql.Tx, claim control.TrainingJob) error {
	saved, status, err := readTrainingClaim(ctx, tx, claim.ID)
	if err != nil {
		return err
	}
	if status != "preparing" || claim.Token == "" || saved.Token != claim.Token || saved.Workload != claim.Workload || saved.CatalogRevision != claim.CatalogRevision {
		return ErrStaleFence
	}
	var admitted bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM registered_work WHERE request_id=?) OR EXISTS(SELECT 1 FROM broker_demands WHERE request_id=?)`, claim.ID, claim.ID).Scan(&admitted); err != nil {
		return err
	}
	if admitted {
		return ErrRequestConflict
	}
	return nil
}

func (s *Store) trainingClaimReservationSource(ctx context.Context, tx *sql.Tx, current control.State, claim control.TrainingJob) (control.WorkloadProfile, error) {
	if err := checkTrainingClaim(ctx, tx, claim); err != nil {
		return control.WorkloadProfile{}, err
	}
	return s.trainingReservationSource(ctx, tx, current, claim.Workload, claim.CatalogRevision)
}

// AbortTrainingPreparation leaves a permanent interrupted identity tombstone.
// It cannot cancel a consumed claim and never changes GPU ownership or admission.
func (s *Store) AbortTrainingPreparation(ctx context.Context, claim control.TrainingJob) (control.State, error) {
	return s.stateTx(ctx, func(tx *sql.Tx, current control.State) (control.State, error) {
		if err := checkTrainingClaim(ctx, tx, claim); err != nil {
			return current, err
		}
		claim.Status = "interrupted"
		claim.UpdatedAt = s.now().UTC()
		raw, err := json.Marshal(claim)
		if err != nil {
			return current, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE training_claims SET payload=?,status='interrupted' WHERE id=?`, raw, claim.ID)
		return current, err
	})
}

func rejectTrainingClaim(ctx context.Context, tx *sql.Tx, id string) error {
	var claimed bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM training_claims WHERE id=?)`, id).Scan(&claimed); err != nil {
		return err
	}
	if claimed {
		return ErrRequestConflict
	}
	return nil
}
