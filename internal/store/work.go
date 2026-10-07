package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

var ErrRequestConflict = errors.New("request id already exists")
var ErrRegistrationTokenMismatch = errors.New("registration token does not match work")

// ErrWorkAlreadyCompleted reports an idempotent finish retry: request, workload,
// fence and token all match a registration that already reached a terminal state.
var ErrWorkAlreadyCompleted = errors.New("work already completed")

type WorkOutcome string

const (
	WorkCompleted WorkOutcome = "completed"
	WorkAbandoned WorkOutcome = "abandoned"
)

var ErrNoUnfinishedWork = errors.New("no unfinished work to resolve")
var ErrAdmissionOpen = errors.New("admission must be closed for work resolution")

func ValidateResolutionReason(reason string) (string, error) {
	reason = strings.TrimSpace(reason)
	if len(reason) == 0 || len(reason) > 512 {
		return "", errors.New("resolution reason must contain 1 to 512 bytes")
	}
	return reason, nil
}

// ResolveUnfinishedWork is reserved for verified operator recovery. The caller
// must quiesce all proxy processes and stop/verify both runtimes first. This
// transaction requires the closed, rotated state version and records the
// resolution together with the terminal updates.
func (s *Store) ResolveUnfinishedWork(ctx context.Context, expected uint64, reason string) (int64, error) {
	reason, err := ValidateResolutionReason(reason)
	if err != nil {
		return 0, err
	}
	var count int64
	_, err = s.withStateTx(ctx, expected, func(tx *sql.Tx, state control.State) (control.State, error) {
		var err error
		count, err = s.resolveUnfinishedWork(ctx, tx, state, reason)
		return state, err
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Store) resolveUnfinishedWork(ctx context.Context, tx *sql.Tx, state control.State, reason string) (int64, error) {
	if state.Admission != control.AdmissionClosed {
		return 0, ErrAdmissionOpen
	}
	if state.Owner != control.OwnerSupervisor {
		return 0, errors.New("supervisor ownership required for work resolution")
	}
	var unfenced int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM registered_work
		WHERE completed_at IS NULL AND lease_incarnation = ? AND lease_epoch >= ?`,
		state.LeaseFence.Incarnation, state.LeaseFence.Epoch).Scan(&unfenced); err != nil {
		return 0, err
	}
	if unfenced != 0 {
		return 0, ErrStaleFence
	}
	now := formatTime(s.now())
	result, err := tx.ExecContext(ctx, `UPDATE registered_work
		SET completed_at = ?, completion_outcome = 'abandoned'
		WHERE completed_at IS NULL`, now)
	if err != nil {
		return 0, fmt.Errorf("resolve unfinished work: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if count == 0 {
		return 0, ErrNoUnfinishedWork
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO work_resolutions
		(lease_incarnation, lease_epoch, state_version, reason, abandoned_work, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, state.LeaseFence.Incarnation,
		state.LeaseFence.Epoch, state.Version, reason, count, now); err != nil {
		return 0, fmt.Errorf("audit unfinished work resolution: %w", err)
	}
	return count, nil
}

func (s *Store) beginAdmittedWork(ctx context.Context, requestID string, workload control.Workload, fence control.Fence, revision *string) (*sql.Tx, error) {
	if err := control.ValidateRequestID(requestID); err != nil {
		return nil, err
	}
	if err := fence.Validate(); err != nil {
		return nil, fmt.Errorf("invalid fence: %w", err)
	}
	if !control.ValidWorkloadID(workload) {
		return nil, errors.New("invalid workload")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	admitted := false
	defer func() {
		if !admitted {
			_ = tx.Rollback()
		}
	}()
	if err := checkAdmissibleState(ctx, tx, workload, fence, revision); err != nil {
		return nil, err
	}
	admitted = true
	return tx, nil
}

func checkAdmissibleState(ctx context.Context, tx *sql.Tx, workload control.Workload, fence control.Fence, revision *string) error {
	catalog, err := readCatalog(ctx, tx)
	if err != nil {
		return err
	}
	if revision != nil && catalog.Revision != *revision {
		return ErrVersionConflict
	}
	if err := catalogAdmitsWorkload(catalog, workload); err != nil {
		return err
	}
	state, err := readState(ctx, tx)
	if err != nil {
		return err
	}
	if state.LeaseFence != fence {
		return ErrStaleFence
	}
	if state.Admission != control.AdmissionOpen || state.Phase != control.PhaseStable || state.Health != control.HealthHealthy {
		return ErrAdmissionClosed
	}
	if state.ActiveWorkload != workload || state.DesiredWorkload != workload {
		return ErrWorkloadMismatch
	}
	return nil
}

func (s *Store) AdmitWorkToken(ctx context.Context, requestID, jobID string, workload control.Workload, fence control.Fence) (string, error) {
	token, err := control.NewUUID()
	if err != nil {
		return "", err
	}
	if err := s.admitWorkTx(ctx, requestID, jobID, workload, fence, admissionBinding{token: token}); err != nil {
		return "", err
	}
	return token, nil
}

// AdmitWorkTokenAtCatalog binds admission and catalog verification to one transaction.
func (s *Store) AdmitWorkTokenAtCatalog(ctx context.Context, requestID, jobID string, workload control.Workload, fence control.Fence, revision string) (string, error) {
	token, err := control.NewUUID()
	if err != nil {
		return "", err
	}
	if err := s.admitWorkTx(ctx, requestID, jobID, workload, fence, admissionBinding{token: token, revision: &revision}); err != nil {
		return "", err
	}
	return token, nil
}

type admissionBinding struct {
	token    string
	revision *string
}

func (s *Store) admitWorkTx(ctx context.Context, requestID, jobID string, workload control.Workload, fence control.Fence, binding admissionBinding) error {
	tx, err := s.beginAdmittedWork(ctx, requestID, workload, fence, binding.revision)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO registered_work
		(request_id, job_id, workload, lease_incarnation, lease_epoch, registered_at, registration_token)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		requestID, nullable(jobID), workload, fence.Incarnation, fence.Epoch, formatTime(s.now()), nullable(binding.token)); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint") {
			return ErrRequestConflict
		}
		return fmt.Errorf("admit work: %w", err)
	}
	if err := s.touchActivity(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) FinishWorkToken(ctx context.Context, requestID string, workload control.Workload, fence control.Fence, token string, outcome WorkOutcome) error {
	if err := validateFinishedWork(requestID, workload, fence, outcome); err != nil {
		return err
	}
	// Rows registered before completion tokens existed have a NULL
	// registration_token; only an empty presented token may finish them.
	// A successful finish records activity in the same writer transaction.
	finished := false
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE registered_work
			SET completed_at = ?, completion_outcome = ?
			WHERE request_id = ? AND completed_at IS NULL
			  AND workload = ? AND lease_incarnation = ? AND lease_epoch = ?
			  AND ((registration_token IS NULL AND ? = '') OR registration_token = ?)`,
			formatTime(s.now()), outcome, requestID, workload, fence.Incarnation, fence.Epoch, token, token)
		if err != nil {
			return fmt.Errorf("finish work: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return nil
		}
		if err := s.touchActivity(ctx, tx); err != nil {
			return err
		}
		finished = true
		return nil
	})
	if err != nil {
		return err
	}
	if finished {
		return nil
	}
	var registeredWorkload sql.NullString
	var incarnation string
	var epoch uint64
	var registeredToken sql.NullString
	var completedAt sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT workload, lease_incarnation, lease_epoch, registration_token, completed_at
		FROM registered_work WHERE request_id = ?`, requestID).Scan(&registeredWorkload, &incarnation, &epoch, &registeredToken, &completedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return sql.ErrNoRows
	}
	if err != nil {
		return err
	}
	if !registeredWorkload.Valid || control.Workload(registeredWorkload.String) != workload {
		return ErrWorkloadMismatch
	}
	if incarnation != fence.Incarnation || epoch != fence.Epoch {
		return ErrStaleFence
	}
	if registeredToken.Valid && registeredToken.String != token || !registeredToken.Valid && token != "" {
		return ErrRegistrationTokenMismatch
	}
	if completedAt.Valid {
		return ErrWorkAlreadyCompleted
	}
	return sql.ErrNoRows
}

func validateFinishedWork(requestID string, workload control.Workload, fence control.Fence, outcome WorkOutcome) error {
	if requestID == "" {
		return errors.New("request id is empty")
	}
	if err := fence.Validate(); err != nil {
		return fmt.Errorf("invalid fence: %w", err)
	}
	if !control.ValidWorkloadID(workload) {
		return errors.New("invalid workload")
	}
	if outcome != WorkCompleted && outcome != WorkAbandoned {
		return errors.New("invalid work outcome")
	}
	return nil
}

// julianday compares timestamps at subsecond precision: a plain string compare
// misorders fractional seconds and unixepoch truncates to whole seconds.
const prunableWorkIDs = `SELECT work.request_id FROM registered_work AS work
	JOIN control_state AS state ON state.singleton = 1
	WHERE work.completed_at IS NOT NULL
	  AND (work.registration_token IS NOT NULL OR work.lease_incarnation <> state.lease_incarnation OR work.lease_epoch <> state.lease_epoch)
	  AND julianday(work.completed_at) < julianday(?)
	  AND NOT EXISTS (
		SELECT 1 FROM transition_work AS snapshot
		JOIN transitions AS tr ON tr.transition_id = snapshot.transition_id
		WHERE snapshot.request_id = work.request_id AND tr.status = 'in_progress'
	  )
	ORDER BY work.completed_at, work.request_id LIMIT ?`

func selectPrunableWorkIDs(ctx context.Context, tx *sql.Tx, cutoff string, limit int) ([]string, error) {
	rows, err := tx.QueryContext(ctx, prunableWorkIDs, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("select completed work to prune: %w", err)
	}
	return readPrunableWorkIDs(rows)
}

func readPrunableWorkIDs(rows *sql.Rows) ([]string, error) {
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan completed work to prune: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("read completed work to prune: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close completed work selection: %w", err)
	}
	return ids, nil
}

func (s *Store) PruneCompletedWork(ctx context.Context, before time.Time, limit int) (int64, error) {
	if before.IsZero() {
		return 0, errors.New("retention cutoff is required")
	}
	if limit < 1 || limit > 1024 {
		return 0, errors.New("prune limit must be between 1 and 1024")
	}
	cutoff := formatTime(before)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	ids, err := selectPrunableWorkIDs(ctx, tx, cutoff, limit)
	if err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM transition_work
		WHERE request_id IN (`+placeholders+`)`, args...); err != nil {
		return 0, fmt.Errorf("prune terminal transition snapshots: %w", err)
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM registered_work
		WHERE request_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return 0, fmt.Errorf("prune completed work: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return deleted, nil
}

// PendingWork reports whether any registration still has completion authority.
// Recovery closes admission before polling, so this includes all pre-entry work.
func (s *Store) PendingWork(ctx context.Context) (int, error) {
	var pending int
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM registered_work WHERE completed_at IS NULL)`).Scan(&pending)
	return pending, err
}

// NULL legacy workload identities cannot be attributed to the retained target.
func (s *Store) PendingWorkExcept(ctx context.Context, retained control.Workload) (int, error) {
	var pending int
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM registered_work WHERE completed_at IS NULL AND (workload IS NULL OR workload <> ?))`, retained).Scan(&pending)
	return pending, err
}

func catalogAdmitsWorkload(catalog control.CatalogSnapshot, workload control.Workload) error {
	if catalog.Revision != "" {
		if _, ok := catalog.Catalog.Profile(workload); !ok {
			return ErrWorkloadMismatch
		}
	} else if workload != control.WorkloadText && workload != control.WorkloadMedia {
		return ErrWorkloadMismatch
	}
	return nil
}
