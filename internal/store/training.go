package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

var ErrTrainingPending = errors.New("unfinished training job requires training reconciliation or explicit training recovery")

const schemaV16 = `CREATE TABLE training_jobs (
 id TEXT PRIMARY KEY REFERENCES registered_work(request_id),
 payload BLOB NOT NULL,
 finished INTEGER NOT NULL DEFAULT 0 CHECK(finished IN (0,1))
); CREATE UNIQUE INDEX one_unfinished_training ON training_jobs(finished) WHERE finished=0;`

func trainingPending(ctx context.Context, tx *sql.Tx) error {
	var pending bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM training_jobs WHERE finished=0)`).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return ErrTrainingPending
	}
	return nil
}

func (s *Store) TrainingJob(ctx context.Context, id string) (control.TrainingJob, error) {
	return readTrainingJob(ctx, s.db, id)
}

func readTrainingJob(ctx context.Context, q querier, id string) (control.TrainingJob, error) {
	var job control.TrainingJob
	var raw []byte
	if err := q.QueryRowContext(ctx, `SELECT payload FROM training_jobs WHERE id=?`, id).Scan(&raw); err != nil {
		return job, err
	}
	err := json.Unmarshal(raw, &job)
	return job, err
}

// ReserveTraining atomically claims the idle GPU and registers all work before
// any trainer effect. A job identity can never be reused, even after failure.
func (s *Store) ReserveTraining(ctx context.Context, id string, workload control.Workload, revision string) (control.TrainingJob, error) {
	var job control.TrainingJob
	if err := control.ValidateRequestID(id); err != nil {
		return job, err
	}
	token, err := s.uuid()
	if err != nil {
		return job, err
	}
	_, err = s.stateTx(ctx, func(tx *sql.Tx, current control.State) (control.State, error) {
		p, err := s.trainingReservationSource(ctx, tx, current, workload, revision)
		if err != nil {
			return current, err
		}
		current.LeaseFence.Epoch++
		current.Version++
		current.UpdatedAt = s.now().UTC()
		current.ActiveWorkload = workload
		current.DesiredWorkload = workload
		current.PendingIdleDeadline = nil
		job = control.TrainingJob{ID: id, Token: token, Workload: workload, Fence: current.LeaseFence, CatalogRevision: revision, RecipeSHA256: p.Trainer.RecipeSHA256, Status: "launching", UpdatedAt: current.UpdatedAt}
		if _, err := tx.ExecContext(ctx, `INSERT INTO registered_work(request_id,job_id,workload,lease_incarnation,lease_epoch,registered_at,registration_token) VALUES(?,?,?,?,?,?,?)`, id, id, workload, job.Fence.Incarnation, job.Fence.Epoch, formatTime(s.now()), token); err != nil {
			if isUniqueConstraint(err) {
				return current, ErrRequestConflict
			}
			return current, err
		}
		raw, err := json.Marshal(job)
		if err != nil {
			return current, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO training_jobs(id,payload) VALUES(?,?)`, id, raw); err != nil {
			return current, err
		}
		if err := s.disarmIdleDeadline(ctx, tx); err != nil {
			return current, err
		}
		return current, writeState(ctx, tx, current)
	})
	return job, err
}

func checkTrainingJob(ctx context.Context, tx *sql.Tx, current control.State, job control.TrainingJob) (control.TrainingJob, error) {
	stored, err := readTrainingJob(ctx, tx, job.ID)
	if err != nil {
		return stored, err
	}
	if stored.Token != job.Token || stored.Fence != job.Fence || current.LeaseFence != job.Fence || control.TrainingTerminalOutcome(stored.Status) {
		return stored, ErrStaleFence
	}
	return stored, nil
}

func (s *Store) RecordTrainingLaunch(ctx context.Context, job control.TrainingJob, invocation string) error {
	decoded, decodeErr := hex.DecodeString(invocation)
	if decodeErr != nil || len(decoded) != 16 {
		return errors.New("missing trainer invocation evidence")
	}
	_, err := s.stateTx(ctx, func(tx *sql.Tx, current control.State) (control.State, error) {
		stored, err := checkTrainingJob(ctx, tx, current, job)
		if err != nil {
			return current, err
		}
		if stored.Status != "launching" || stored.InvocationID != "" {
			return current, ErrStaleFence
		}
		stored.Status = "running"
		stored.InvocationID = invocation
		stored.UpdatedAt = s.now().UTC()
		return current, writeTrainingJob(ctx, tx, stored, false)
	})
	return err
}

func writeTrainingJob(ctx context.Context, tx *sql.Tx, job control.TrainingJob, finished bool) error {
	raw, err := json.Marshal(job)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE training_jobs SET payload=?,finished=? WHERE id=?`, raw, finished, job.ID)
	return err
}

// FinishTraining is called only after terminal and release proofs under the
// controller gate. Finishing work and returning idle are one transaction.
func (s *Store) FinishTraining(ctx context.Context, job control.TrainingJob, outcome string) (control.State, error) {
	if !control.TrainingTerminalOutcome(outcome) {
		return control.State{}, errors.New("invalid training outcome")
	}
	return s.stateTx(ctx, func(tx *sql.Tx, current control.State) (control.State, error) {
		stored, err := checkTrainingJob(ctx, tx, current, job)
		if err != nil {
			return current, err
		}
		stored.Status = outcome
		stored.UpdatedAt = s.now().UTC()
		if err := writeTrainingJob(ctx, tx, stored, true); err != nil {
			return current, err
		}
		workOutcome := "abandoned"
		if outcome == "completed" {
			workOutcome = "completed"
		}
		if _, err := tx.ExecContext(ctx, `UPDATE registered_work SET completed_at=?,completion_outcome=? WHERE request_id=? AND completed_at IS NULL`, formatTime(s.now()), workOutcome, job.ID); err != nil {
			return current, err
		}
		current.ActiveWorkload = control.WorkloadIdle
		current.DesiredWorkload = control.WorkloadIdle
		current.Phase = control.PhaseStable
		current.Health = control.HealthHealthy
		current.Admission = control.AdmissionClosed
		current.LeaseFence.Epoch++
		current.Version++
		current.UpdatedAt = s.now().UTC()
		current.PendingIdleDeadline = nil
		if err := s.disarmIdleDeadline(ctx, tx); err != nil {
			return current, err
		}
		return current, writeState(ctx, tx, current)
	})
}

func (s *Store) trainingReservationSource(ctx context.Context, tx *sql.Tx, current control.State, workload control.Workload, revision string) (control.WorkloadProfile, error) {
	if err := temporaryDiscoveryPending(ctx, tx); err != nil {
		return control.WorkloadProfile{}, err
	}
	catalog, err := readCatalog(ctx, tx)
	if err != nil {
		return control.WorkloadProfile{}, err
	}
	p, ok := catalog.Catalog.Profile(workload)
	if !ok || p.Trainer == nil || catalog.Revision != revision {
		return control.WorkloadProfile{}, ErrVersionConflict
	}
	if err := s.validateTrainingOutputPaths(*p.Trainer); err != nil {
		return control.WorkloadProfile{}, err
	}
	if current.Owner != control.OwnerSupervisor || current.ActiveWorkload != control.WorkloadIdle || current.DesiredWorkload != control.WorkloadIdle || current.Phase != control.PhaseStable || current.Health != control.HealthHealthy || current.Admission != control.AdmissionClosed {
		return control.WorkloadProfile{}, ErrUnstableState
	}
	var busy bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM registered_work WHERE completed_at IS NULL) OR EXISTS(SELECT 1 FROM transitions WHERE status='in_progress')`).Scan(&busy); err != nil {
		return control.WorkloadProfile{}, err
	}
	if busy {
		return control.WorkloadProfile{}, ErrTrainingPending
	}
	return p, nil
}

func (s *Store) validateTrainingOutputPaths(t control.TrainerBinding) error {
	statePath, err := filepath.Abs(s.statePath)
	if err != nil {
		return err
	}
	for _, output := range []string{t.JobFile, t.TerminalFile} {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal", ".lock", ".proxy.lock", ".user-execution.lock", ".deployment.json"} {
			if output == statePath+suffix {
				return errors.New("trainer output overlaps control state")
			}
		}
	}
	return nil
}
