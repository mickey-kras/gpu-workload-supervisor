package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

const MaxComfyUIAttempts = 4096

var ErrComfyUILimit = errors.New("unfinished ComfyUI attempt limit reached")

// ComfyUIAttempt holds lifecycle correlation only. Graphs, inputs and outputs
// remain with the caller/runtime; they are never persisted by the supervisor.
type ComfyUIAttempt struct {
	AttemptID, PromptID, Upstream, Contract string
	RequestID, RegistrationToken            string
	Workload                                control.Workload
	Fence                                   control.Fence
}

func canonicalUUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id.String() == value && id != uuid.Nil
}

func validateComfyUIAttempt(attempt ComfyUIAttempt) error {
	if !canonicalUUID(attempt.AttemptID) || !canonicalUUID(attempt.PromptID) || len(attempt.Contract) == 0 || len(attempt.Contract) > 128 {
		return errors.New("invalid ComfyUI correlation")
	}
	u, err := url.Parse(attempt.Upstream)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || len(attempt.Upstream) > 2048 {
		return errors.New("invalid ComfyUI upstream")
	}
	return nil
}

// AdmitComfyUIWork commits the registration and its pre-enqueue identity in
// one transaction. No caller may dispatch /prompt until this returns success.
func (s *Store) AdmitComfyUIWork(ctx context.Context, attempt ComfyUIAttempt, revision string) (string, error) {
	if err := validateComfyUIAttempt(attempt); err != nil {
		return "", err
	}
	token, err := control.NewUUID()
	if err != nil {
		return "", err
	}
	tx, err := s.beginAdmittedWork(ctx, attempt.RequestID, attempt.Workload, attempt.Fence, &revision)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM comfyui_attempts AS a
		JOIN registered_work AS w ON w.request_id=a.request_id WHERE w.completed_at IS NULL`).Scan(&count); err != nil {
		return "", err
	}
	if count >= MaxComfyUIAttempts {
		return "", ErrComfyUILimit
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO registered_work
		(request_id,workload,lease_incarnation,lease_epoch,registered_at,registration_token)
		VALUES (?,?,?,?,?,?)`, attempt.RequestID, attempt.Workload, attempt.Fence.Incarnation, attempt.Fence.Epoch, formatTime(s.now()), token); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint") {
			return "", ErrRequestConflict
		}
		return "", fmt.Errorf("admit ComfyUI work: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO comfyui_attempts
		(request_id,attempt_id,prompt_id,upstream,contract) VALUES (?,?,?,?,?)`, attempt.RequestID, attempt.AttemptID, attempt.PromptID, attempt.Upstream, attempt.Contract); err != nil {
		return "", fmt.Errorf("persist ComfyUI attempt: %w", err)
	}
	if err := s.touchActivity(ctx, tx); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return token, nil
}

// PendingComfyUIAttempts uses a stable cursor so an unresolved attempt cannot
// starve later attempts. The caller wraps its cursor after reaching the end.
func (s *Store) PendingComfyUIAttempts(ctx context.Context, workload control.Workload, upstream, contract, after string, limit int) ([]ComfyUIAttempt, error) {
	if limit < 1 || limit > 64 {
		return nil, errors.New("ComfyUI poll limit must be between 1 and 64")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT a.attempt_id,a.prompt_id,a.upstream,a.contract,
		w.request_id,w.registration_token,w.workload,w.lease_incarnation,w.lease_epoch
		FROM comfyui_attempts AS a JOIN registered_work AS w ON w.request_id=a.request_id
		WHERE w.completed_at IS NULL AND w.workload=? AND a.upstream=? AND a.contract=? AND a.attempt_id>?
		ORDER BY a.attempt_id LIMIT ?`, workload, upstream, contract, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var attempts []ComfyUIAttempt
	for rows.Next() {
		var a ComfyUIAttempt
		if err := rows.Scan(&a.AttemptID, &a.PromptID, &a.Upstream, &a.Contract, &a.RequestID, &a.RegistrationToken, &a.Workload, &a.Fence.Incarnation, &a.Fence.Epoch); err != nil {
			return nil, err
		}
		attempts = append(attempts, a)
	}
	return attempts, rows.Err()
}
