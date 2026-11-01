package setup

import (
	"context"
	"errors"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

type TrainerCandidate struct {
	Workload            control.Workload `json:"workload"`
	Unit                string           `json:"unit"`
	BindingStatus       string           `json:"bindingStatus"`
	QualificationStatus string           `json:"qualificationStatus"`
	NextStep            string           `json:"nextStep"`
}

// InspectTrainer inspects an explicit trainer binding without applying it.
// Runtime approval and measured capacity cannot be inferred from installed
// executables.
func (b Backend) InspectTrainer(ctx context.Context, request Request, target control.Workload) (TrainerCandidate, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	result := TrainerCandidate{Workload: target, BindingStatus: "unverified", QualificationStatus: "unqualified", NextStep: "Provide an explicit trainer binding and measured capacity before inspecting its launch."}
	if ctx.Err() != nil {
		return result, errors.New("trainer inspection cancelled")
	}
	if err := validateShape(request); err != nil {
		return result, errors.New("invalid trainer inspection request")
	}
	p, ok := request.Catalog.Profile(target)
	if !ok || p.Trainer == nil {
		return result, errors.New("trainer inspection requires an explicit trainer profile")
	}
	result.Unit = p.Unit
	result.BindingStatus = inspectionFailedStatus
	result.NextStep = "Trainer launch verification failed. Check the explicit binding and loaded service, then retry."
	manager, err := b.makeRuntime(request)
	if err != nil {
		return result, errors.New("trainer inspection runtime is unavailable")
	}
	inspector, ok := manager.(interface {
		InspectTrainer(context.Context, control.Workload) error
	})
	if !ok {
		return result, errors.New("runtime does not support trainer inspection")
	}
	if err := inspector.InspectTrainer(ctx, target); err != nil || ctx.Err() != nil {
		return result, errors.New("trainer binding inspection failed")
	}
	result.BindingStatus = "verified"
	result.NextStep = "Launch binding verified. Complete private runtime and real GPU qualification before enabling training."
	return result, nil
}

func (b Backend) discoverTrainers(ctx context.Context, result *Discovery) {
	result.Trainers = []TrainerCandidate{}
	for _, p := range result.Request.Catalog.Profiles {
		if p.Trainer == nil {
			continue
		}
		candidate, err := b.InspectTrainer(ctx, result.Request, p.ID)
		if err != nil {
			candidate.NextStep = err.Error() + ". " + candidate.NextStep
		}
		result.Trainers = append(result.Trainers, candidate)
	}
}
