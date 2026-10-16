package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/supervisor"
)

func executeTraining(ctx context.Context, c *supervisor.Controller, command, id string, target control.Workload) error {
	if err := control.ValidateRequestID(id); err != nil {
		return err
	}
	if command == "training-start" || command == "training-status" {
		var job control.TrainingJob
		var err error
		if command == "training-start" {
			job, err = c.StartTraining(ctx, id, target)
		} else {
			job, err = c.TrainingStatus(ctx, id)
		}
		// Completion tokens stay in the private lifecycle envelope/store.
		job.Token = ""
		return errors.Join(err, json.NewEncoder(os.Stdout).Encode(job))
	}
	var state control.State
	var err error
	switch command {
	case "training-reconcile":
		state, err = c.ReconcileTraining(ctx, id)
	case "training-recover":
		state, err = c.RecoverTraining(ctx, id)
	default:
		return errors.New("unknown training command")
	}
	return errors.Join(err, json.NewEncoder(os.Stdout).Encode(state))
}

func validateTrainingFlags(command, id string) error {
	switch command {
	case "training-start", "training-status", "training-reconcile", "training-recover":
		return control.ValidateRequestID(id)
	default:
		if strings.HasPrefix(command, "training-") {
			return errors.New("unknown training command")
		}
		if id != "" {
			return errors.New("-training-job requires a training command")
		}
		return nil
	}
}
