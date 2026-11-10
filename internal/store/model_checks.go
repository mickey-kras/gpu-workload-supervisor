package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/modelcheck"
)

// schemaV18 keeps one model check result for each model. The supervisor
// writes each payload itself after strict validation.
const schemaV18 = `CREATE TABLE model_checks(
 model TEXT PRIMARY KEY CHECK(length(model) BETWEEN 1 AND 128),
 payload BLOB NOT NULL CHECK(length(payload) BETWEEN 1 AND 4194304));`

// ErrNoModelCheck means that a model has no stored check result.
var ErrNoModelCheck = errors.New("model has no stored check result")

// PutModelCheck replaces the stored result of record.Model. A run can read
// the stored result before an operator approves its review list, and
// write after the approval. So, in one transaction, it reads the stored
// result again. When the new result asks for review and the stored result
// has an approval for the same root, source, template and review list,
// the new result keeps that approval.
func (s *Store) PutModelCheck(ctx context.Context, record modelcheck.Record) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		stored, _, err := readModelCheck(ctx, tx, record.Model)
		if err == nil {
			record = keepApproval(record, stored)
		}
		raw, err := modelcheck.MarshalRecord(record)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO model_checks(model,payload) VALUES(?,?) ON CONFLICT(model) DO UPDATE SET payload=excluded.payload`, record.Model, raw)
		return err
	})
}

// keepApproval gives record with the approval of stored, when record asks
// for review and the approval is for the same root, source, template and
// review list. Else it gives record.
func keepApproval(record, stored modelcheck.Record) modelcheck.Record {
	if stored.TemplateApproval == nil || stored.Root != record.Root || stored.Source != record.Source {
		return record
	}
	if approved, err := modelcheck.Approve(record, *stored.TemplateApproval); err == nil {
		return approved
	}
	return record
}

// ModelCheck reads the stored result of one model. A result of another
// record version gives ErrNoModelCheck, so the model is not scanned. A stored
// result that is not valid is an error, never a pass.
func (s *Store) ModelCheck(ctx context.Context, model string) (modelcheck.Record, error) {
	record, _, err := readModelCheck(ctx, s.db, model)
	return record, err
}

// readModelCheck reads the stored result of one model and its stored
// payload, with the rules of ModelCheck.
func readModelCheck(ctx context.Context, db querier, model string) (modelcheck.Record, []byte, error) {
	var raw []byte
	err := db.QueryRowContext(ctx, `SELECT payload FROM model_checks WHERE model=?`, model).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return modelcheck.Record{}, nil, ErrNoModelCheck
	}
	if err != nil {
		return modelcheck.Record{}, nil, err
	}
	record, err := modelcheck.ReadRecord(raw)
	if errors.Is(err, modelcheck.ErrStaleRecord) {
		// A result of another record version does not apply: not scanned.
		return modelcheck.Record{}, nil, ErrNoModelCheck
	}
	if err != nil || record.Model != model {
		return modelcheck.Record{}, nil, errors.New("stored model check result is invalid")
	}
	return record, raw, nil
}

// DeleteModelCheck removes the stored result of one model, for example when
// it is no longer selected.
func (s *Store) DeleteModelCheck(ctx context.Context, model string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM model_checks WHERE model=?`, model)
	return err
}

// ApproveTemplate stores the approval of an operator for the template
// review of the stored result of model. The stored result must ask for that
// review, else it gives modelcheck.ErrNoPendingReview. The approval must be
// valid and match its template and review list, else it gives
// modelcheck.ErrApprovalMismatch. A model without a current stored result
// gives ErrNoModelCheck. It updates the result only when no other write
// changed it since the read; else it gives ErrModelCheckConflict.
func (s *Store) ApproveTemplate(ctx context.Context, model string, approval modelcheck.TemplateApproval) error {
	record, raw, err := readModelCheck(ctx, s.db, model)
	if err != nil {
		return err
	}
	approved, err := modelcheck.Approve(record, approval)
	if err != nil {
		return err
	}
	next, err := modelcheck.MarshalRecord(approved)
	if err != nil {
		return err
	}
	return s.replaceModelCheck(ctx, model, raw, next)
}

// replaceModelCheck replaces the stored payload of model with next only
// when it is still old.
func (s *Store) replaceModelCheck(ctx context.Context, model string, old, next []byte) error {
	result, err := s.db.ExecContext(ctx, `UPDATE model_checks SET payload=? WHERE model=? AND payload=?`, next, model, old)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	switch {
	case err != nil:
		return err
	case n != 1:
		return ErrModelCheckConflict
	}
	return nil
}

// ErrModelCheckConflict means that another write, such as a new run,
// changed a stored result during an approval.
var ErrModelCheckConflict = errors.New("the stored model check result changed during the approval; read the result again before you approve")

// TemplateConfirm shows a review list to an operator and gives the UID of
// the operator who approved it. It gives an error when the operator did not
// approve.
type TemplateConfirm func(ctx context.Context, request modelcheck.ReviewRequest) (int, error)

// TemplateApprover gives an approver for modelcheck.Checker. It reuses the
// stored approval of the same model, root, source, template and review
// list. The approval is a part of the stored result, so a new result that
// does not reach the template check drops it. A result deleted before a run
// starts drops it too. A run that read the approval before the delete
// stores it again. A run that read the result before an approval keeps
// the approval when it writes, see PutModelCheck. Else it asks confirm. Without confirm, it refuses with
// modelcheck.ErrApprovalNeeded: no approval is ever automatic. A confirm
// that the operator refused must give an error that wraps
// modelcheck.ErrApprovalRefused. A store read error, other than no stored
// result, is an error and not a request for review.
func (s *Store) TemplateApprover(confirm TemplateConfirm, now func() time.Time) modelcheck.Approver {
	if now == nil {
		now = time.Now
	}
	return func(ctx context.Context, request modelcheck.ReviewRequest) (modelcheck.TemplateApproval, error) {
		stored, err := s.ModelCheck(ctx, request.Model)
		if err != nil && !errors.Is(err, ErrNoModelCheck) {
			return modelcheck.TemplateApproval{}, err
		}
		if err == nil && stored.TemplateApproval != nil && stored.Root == request.Root && stored.Source == request.Source && stored.TemplateApproval.Matches(request) {
			return *stored.TemplateApproval, nil
		}
		if confirm == nil {
			return modelcheck.TemplateApproval{}, modelcheck.ErrApprovalNeeded
		}
		uid, err := confirm(ctx, request)
		if err != nil {
			return modelcheck.TemplateApproval{}, err
		}
		return modelcheck.NewApproval(request, uid, now()), nil
	}
}
