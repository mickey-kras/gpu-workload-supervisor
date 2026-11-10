package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/supervisor"
	_ "modernc.org/sqlite"
)

const trainingRecoverCommand = "training-recover"

// restoredTrainingQuery lists the unfinished training of restored state:
// reserved jobs block restore-state, and preparing claims reserve no GPU.
// It excludes finished jobs and interrupted claims.
const restoredTrainingQuery = `SELECT id, 'reserved job' FROM training_jobs WHERE finished=0
UNION ALL
SELECT id, 'preparing claim' FROM training_claims WHERE status='preparing'
ORDER BY 1`

type restoredTraining struct {
	ID       string `json:"trainingJob"`
	Category string `json:"category"`
}

type restoredTrainingResult struct {
	restoredTraining
	State control.State `json:"state"`
}

// recoverAllRestored runs training-recover for each unfinished training ID
// of restored state, in ID order. It stops at the first failure, so the
// operator investigates before the next recovery. Recovery keeps persisted
// terminal outcomes, otherwise records interrupted, and never resumes or
// relaunches training. All proxies must be stopped, as for restore-state.
func recoverAllRestored(ctx context.Context, c *supervisor.Controller, statePath string) error {
	proxyLock, err := lock.TryAcquire(statePath + ".proxy.lock")
	if err != nil {
		return fmt.Errorf("stop all workload proxies and wait for shutdown before %s -all-restored: %w", trainingRecoverCommand, err)
	}
	defer proxyLock.Close()
	jobs, err := listRestoredTraining(ctx, statePath)
	if err != nil {
		return fmt.Errorf("list unfinished training; keep components stopped and check the state path and schema: %w", err)
	}
	encoder := json.NewEncoder(os.Stdout)
	for _, job := range jobs {
		if err := control.ValidateRequestID(job.ID); err != nil {
			return fmt.Errorf("unfinished %s has an invalid ID: %w", job.Category, err)
		}
		state, err := c.RecoverTraining(ctx, job.ID)
		if err != nil {
			return fmt.Errorf("recover training job %q (%s); keep components stopped, repair, then run %s -all-restored again: %w",
				job.ID, job.Category, trainingRecoverCommand, err)
		}
		if err := encoder.Encode(restoredTrainingResult{job, state}); err != nil {
			return err
		}
	}
	return encoder.Encode(struct {
		Recovered int `json:"recoveredTraining"`
	}{len(jobs)})
}

// listRestoredTraining reads the IDs through a separate read-only
// connection. It reads only IDs and categories, never payloads or tokens.
func listRestoredTraining(ctx context.Context, statePath string) (jobs []restoredTraining, err error) {
	path, err := filepath.Abs(statePath)
	if err != nil {
		return nil, err
	}
	file, err := deployment.OpenPrivate(path)
	if err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	location := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", location.String())
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	rows, err := db.QueryContext(ctx, restoredTrainingQuery)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var job restoredTraining
		if err := rows.Scan(&job.ID, &job.Category); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}
