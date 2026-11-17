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

const schemaV17 = `CREATE TABLE training_jobs (
 id TEXT PRIMARY KEY REFERENCES registered_work(request_id),
 payload BLOB NOT NULL,
 finished INTEGER NOT NULL DEFAULT 0 CHECK(finished IN (0,1))
); CREATE UNIQUE INDEX one_unfinished_training ON training_jobs(finished) WHERE finished=0;
CREATE TABLE training_claims(id TEXT PRIMARY KEY, payload BLOB NOT NULL, status TEXT NOT NULL CHECK(status IN ('preparing','reserved','interrupted')));`

func trainingPending(ctx context.Context, tx *sql.Tx) error {
	var pending bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM training_jobs WHERE finished=0)`).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return ErrTrainingPending
	}
	return temporaryDiscoveryPending(ctx, tx)
}

func (s *Store) TrainingJob(ctx context.Context, id string) (control.TrainingJob, error) {
	return readTrainingJob(ctx, s.db, id)
}

func readTrainingJob(ctx context.Context, q querier, id string) (control.TrainingJob, error) {
	var job control.TrainingJob
	var raw []byte
	if err := q.QueryRowContext(ctx, `SELECT payload FROM training_jobs WHERE id=? UNION ALL SELECT payload FROM training_claims WHERE id=? AND status!='reserved' LIMIT 1`, id, id).Scan(&raw); err != nil {
		return job, err
	}
	err := json.Unmarshal(raw, &job)
	return job, err
}

// ReserveTraining atomically claims the idle GPU and registers all work before
// any trainer effect. A job identity can never be reused, even after failure.
func (s *Store) ReserveTraining(ctx context.Context, claim control.TrainingJob) (control.TrainingJob, error) {
	var job control.TrainingJob
	id, workload, revision, token := claim.ID, claim.Workload, claim.CatalogRevision, claim.Token
	_, err := s.stateTx(ctx, func(tx *sql.Tx, current control.State) (control.State, error) {
		p, err := s.trainingClaimReservationSource(ctx, tx, current, claim)
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
		if _, err := tx.ExecContext(ctx, `UPDATE training_claims SET status='reserved' WHERE id=?`, id); err != nil {
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

// RecordTrainingTerminal preserves validated evidence before cleanup destroys
// systemd's retained invocation. The registered admission remains unfinished.
func (s *Store) RecordTrainingTerminal(ctx context.Context, job control.TrainingJob, evidence control.TrainingTerminal) error {
	_, err := s.stateTx(ctx, func(tx *sql.Tx, current control.State) (control.State, error) {
		stored, err := checkTrainingJob(ctx, tx, current, job)
		if err != nil {
			return current, err
		}
		if !evidence.Matches(stored) || (stored.TerminalOutcome != "" && stored.TerminalOutcome != evidence.Outcome) {
			return current, ErrStaleFence
		}
		stored.TerminalOutcome = evidence.Outcome
		stored.Status = "finishing"
		stored.UpdatedAt = s.now().UTC()
		return current, writeTrainingJob(ctx, tx, stored, false)
	})
	return err
}

// RecordTrainingMemoryPeak keeps the largest memory peak of the trainer unit
// read for the job, from the cgroup or from systemd.
// A retried cleanup can read the same cgroup again; a smaller value is ignored.
func (s *Store) RecordTrainingMemoryPeak(ctx context.Context, job control.TrainingJob, peak uint64) error {
	_, err := s.stateTx(ctx, func(tx *sql.Tx, current control.State) (control.State, error) {
		stored, err := checkTrainingJob(ctx, tx, current, job)
		if err != nil {
			return current, err
		}
		if stored.InvocationID == "" {
			return current, ErrStaleFence
		}
		if stored.MemoryPeakBytes != nil && *stored.MemoryPeakBytes >= peak {
			return current, nil
		}
		stored.MemoryPeakBytes = &peak
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
		if stored.TerminalOutcome != "" && stored.TerminalOutcome != outcome {
			return current, ErrStaleFence
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
	if err := trainingPending(ctx, tx); err != nil {
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
	statePath, err := filepath.EvalSymlinks(s.statePath)
	if err != nil {
		return err
	}
	statePath, err = filepath.Abs(statePath)
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

// CheckTrainingRequestID rejects existing work and demand identities before
// controller side effects. Reservation repeats this check in its transaction.
func (s *Store) CheckTrainingRequestID(ctx context.Context, id string) error {
	if err := control.ValidateRequestID(id); err != nil {
		return err
	}
	return checkTrainingRequestID(ctx, s.db, id)
}

func checkTrainingRequestID(ctx context.Context, q querier, id string) error {
	var exists bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM broker_demands WHERE request_id=?) OR EXISTS(SELECT 1 FROM registered_work WHERE request_id=?) OR EXISTS(SELECT 1 FROM training_claims WHERE id=?)`, id, id, id).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return ErrRequestConflict
	}
	return nil
}

// PreflightTraining checks deterministic catalog and destination errors before
// the controller hands off the GPU. Reservation repeats these checks atomically.
func (s *Store) PreflightTraining(ctx context.Context, id string, workload control.Workload, revision string) error {
	if err := s.CheckTrainingRequestID(ctx, id); err != nil {
		return err
	}
	catalog, err := s.Catalog(ctx)
	if err != nil {
		return err
	}
	p, ok := catalog.Catalog.Profile(workload)
	if !ok || p.Trainer == nil || catalog.Revision != revision {
		return ErrVersionConflict
	}
	return s.validateTrainingOutputPaths(*p.Trainer)
}
