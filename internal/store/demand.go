package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

const MaxPendingDemands = 4096
const MaxDemandRecords = 65536

var ErrDemandConflict = errors.New("demand identity or lifecycle conflict")
var ErrDemandCapacity = errors.New("demand evidence capacity reached")

// Demand binds authenticated identity and payload without persisting credentials
// or request bodies. Queue retries must match every field exactly.
type Demand struct {
	RequestID, Requester, Workflow, BodySHA256 string
	Workload                                   control.Workload
}

type demandClaimKey struct{}

// WithDemandClaim authorizes one reservation's atomic attachment to admission.
func WithDemandClaim(ctx context.Context, claim string) context.Context {
	return context.WithValue(ctx, demandClaimKey{}, claim)
}

func validateDemand(d Demand) error {
	if err := control.ValidateRequestID(d.RequestID); err != nil {
		return err
	}
	if len(d.Requester) == 0 || len(d.Requester) > 128 || len(d.Workflow) == 0 || len(d.Workflow) > 128 || !control.ValidWorkloadID(d.Workload) {
		return errors.New("invalid demand identity")
	}
	digest, err := hex.DecodeString(d.BodySHA256)
	if err != nil || len(digest) != 32 || strings.ToLower(d.BodySHA256) != d.BodySHA256 {
		return errors.New("invalid demand body digest")
	}
	return nil
}

func demandMatches(ctx context.Context, tx *sql.Tx, d Demand) (string, error) {
	var existing Demand
	var status string
	err := tx.QueryRowContext(ctx, `SELECT requester,workflow,body_sha256,workload,status FROM broker_demands WHERE request_id=?`, d.RequestID).Scan(&existing.Requester, &existing.Workflow, &existing.BodySHA256, &existing.Workload, &status)
	if err != nil {
		return "", err
	}
	existing.RequestID = d.RequestID
	if existing != d {
		return "", ErrDemandConflict
	}
	return status, nil
}

func (s *Store) QueueDemand(ctx context.Context, d Demand) error {
	if err := validateDemand(d); err != nil {
		return err
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		status, err := demandMatches(ctx, tx, d)
		if err == nil {
			if status != "queued" {
				return ErrDemandConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := checkDemandCapacity(ctx, tx, d.RequestID); err != nil {
			return err
		}
		now := formatTime(s.now())
		if _, err := tx.ExecContext(ctx, `INSERT INTO broker_demands(request_id,requester,workflow,body_sha256,workload,status,created_at,updated_at) VALUES(?,?,?,?,?,'queued',?,?)`, d.RequestID, d.Requester, d.Workflow, d.BodySHA256, d.Workload, now, now); err != nil {
			return err
		}
		return s.touchActivity(ctx, tx)
	})
}

func (s *Store) ReserveDemand(ctx context.Context, d Demand) (string, error) {
	if err := validateDemand(d); err != nil {
		return "", err
	}
	claim, err := control.NewUUID()
	if err != nil {
		return "", err
	}
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		status, err := demandMatches(ctx, tx, d)
		if err != nil {
			return err
		}
		if status != "queued" {
			return ErrDemandConflict
		}
		state, err := readState(ctx, tx)
		if err != nil {
			return err
		}
		if state.Owner != control.OwnerSupervisor {
			return ErrAdmissionClosed
		}
		_, err = tx.ExecContext(ctx, `UPDATE broker_demands SET status='reserved',claim=?,lease_incarnation=?,lease_epoch=?,updated_at=? WHERE request_id=?`, claim, state.LeaseFence.Incarnation, state.LeaseFence.Epoch, formatTime(s.now()), d.RequestID)
		return err
	})
	if err != nil {
		return "", err
	}
	return claim, nil
}

// RequeueDemand is safe only when the caller knows no dispatch was attempted.
// Admission changes the state atomically, preventing a stale release afterward.
func (s *Store) RequeueDemand(ctx context.Context, requestID, claim string) error {
	if claim == "" {
		return ErrDemandConflict
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE broker_demands SET status='queued',claim=NULL,updated_at=? WHERE request_id=? AND status='reserved' AND claim=?`, formatTime(s.now()), requestID, claim)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return ErrDemandConflict
		}
		return s.touchActivity(ctx, tx)
	})
}

func (s *Store) CancelDemand(ctx context.Context, d Demand) error {
	if err := validateDemand(d); err != nil {
		return err
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		status, err := demandMatches(ctx, tx, d)
		if err != nil {
			return err
		}
		if status != "queued" {
			return ErrDemandConflict
		}
		if _, err := tx.ExecContext(ctx, `UPDATE broker_demands SET status='cancelled',updated_at=? WHERE request_id=?`, formatTime(s.now()), d.RequestID); err != nil {
			return err
		}
		return s.touchActivity(ctx, tx)
	})
}

func bindDemand(ctx context.Context, tx *sql.Tx, requestID string, workload control.Workload) error {
	claim, _ := ctx.Value(demandClaimKey{}).(string)
	var status, storedClaim string
	var storedWorkload control.Workload
	err := tx.QueryRowContext(ctx, `SELECT status,COALESCE(claim,''),workload FROM broker_demands WHERE request_id=?`, requestID).Scan(&status, &storedClaim, &storedWorkload)
	if errors.Is(err, sql.ErrNoRows) && claim == "" {
		return nil
	}
	if err != nil {
		return err
	}
	if claim == "" || status != "reserved" || storedClaim != claim || storedWorkload != workload {
		return ErrDemandConflict
	}
	state, err := readState(ctx, tx)
	if err != nil {
		return err
	}
	if state.Owner != control.OwnerSupervisor {
		return ErrAdmissionClosed
	}
	_, err = tx.ExecContext(ctx, `UPDATE broker_demands SET status='admitted' WHERE request_id=?`, requestID)
	return err
}

func checkDemandCapacity(ctx context.Context, tx *sql.Tx, requestID string) error {
	var active, total, registered int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(status IN ('queued','reserved')),0) FROM broker_demands`).Scan(&total, &active); err != nil {
		return err
	}
	if active >= MaxPendingDemands || total >= MaxDemandRecords {
		return ErrDemandCapacity
	}
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM registered_work WHERE request_id=?) OR EXISTS(SELECT 1 FROM training_claims WHERE id=?)`, requestID, requestID).Scan(&registered); err != nil {
		return err
	}
	if registered != 0 {
		return ErrDemandConflict
	}
	return nil
}

// cancelReservedDemands retires ambiguous reservations only inside verified
// recovery or restore transactions. Queued requests remain eligible for retry.
func cancelReservedDemands(ctx context.Context, tx *sql.Tx, now string) (int64, error) {
	result, err := tx.ExecContext(ctx, `UPDATE broker_demands SET status='cancelled',claim=NULL,updated_at=? WHERE status='reserved'`, now)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
