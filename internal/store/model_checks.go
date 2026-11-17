package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/modelcheck"
)

const schemaV18 = `CREATE TABLE model_checks(
 model TEXT PRIMARY KEY CHECK(length(model) BETWEEN 1 AND 128),
 payload BLOB NOT NULL CHECK(length(payload) BETWEEN 1 AND 4194304));`

// ErrNoModelCheck means that a model has no stored check result.
var ErrNoModelCheck = errors.New("model has no stored check result")

// PutModelCheck validates and stores record keyed by record.Model.
func (s *Store) PutModelCheck(ctx context.Context, record modelcheck.Record) error {
	raw, err := modelcheck.MarshalRecord(record)
	if err != nil {
		return err
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO model_checks(model,payload) VALUES(?,?) ON CONFLICT(model) DO UPDATE SET payload=excluded.payload`, record.Model, raw)
		return err
	})
}

// ModelCheck reads the stored result of one model.
func (s *Store) ModelCheck(ctx context.Context, model string) (modelcheck.Record, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM model_checks WHERE model=?`, model).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return modelcheck.Record{}, ErrNoModelCheck
	}
	if err != nil {
		return modelcheck.Record{}, err
	}
	record, err := modelcheck.ReadRecord(raw)
	if errors.Is(err, modelcheck.ErrStaleRecord) {
		return modelcheck.Record{}, ErrNoModelCheck
	}
	if err != nil || record.Model != model {
		return modelcheck.Record{}, errors.New("stored model check result is invalid")
	}
	return record, nil
}

// DeleteModelCheck removes the stored result of one model.
func (s *Store) DeleteModelCheck(ctx context.Context, model string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM model_checks WHERE model=?`, model)
	return err
}
