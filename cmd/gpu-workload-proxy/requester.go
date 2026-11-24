package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/mickey-kras/llm-workload-supervisor/internal/control"
	"github.com/mickey-kras/llm-workload-supervisor/internal/deployment"
	"github.com/mickey-kras/llm-workload-supervisor/internal/operator"
	workloadproxy "github.com/mickey-kras/llm-workload-supervisor/internal/proxy"
	"github.com/mickey-kras/llm-workload-supervisor/internal/strictjson"
)

func configureRequester(config *workloadproxy.Config, path, statePath string, loadProfile func() (operator.Profile, error)) error {
	if path == "" {
		return nil
	}
	file, err := deployment.OpenPrivate(path)
	if err != nil {
		return err
	}
	defer file.Close()
	var policy workloadproxy.RequesterPolicy
	if err := strictjson.DecodeLimited(file, 1<<20, &policy); err != nil {
		return errors.New("invalid requester policy file")
	}
	profile, err := loadProfile()
	if err != nil {
		return err
	}
	if profile.StatePath != statePath {
		return errors.New("requester ingress state must match the operator profile")
	}
	config.RequesterPolicy = &policy
	service := operator.NativeService(profile)
	config.ActivateDemand = func(ctx context.Context, target control.Workload, state control.State, revision string) (control.Fence, error) {
		return activateRequester(ctx, target, state, revision, service.Handle)
	}
	return nil
}

func activateRequester(ctx context.Context, target control.Workload, state control.State, revision string, handle func(operator.Request) operator.Response) (control.Fence, error) {
	if err := ctx.Err(); err != nil {
		return control.Fence{}, err
	}
	result := handle(operator.Request{ProtocolVersion: 1, RequestID: "gateway-activation", Action: "activate-workload", Target: target,
		Expected: &operator.Expected{Incarnation: state.LeaseFence.Incarnation, Version: strconv.FormatUint(state.Version, 10), Owner: state.Owner, ConfigurationRevision: revision}})
	switch result.Code {
	case operator.OK:
	case operator.Deferred, operator.Busy, operator.StaleState:
		// The supervisor may start the workload later, or a newer state read
		// may allow it now. The waiting request tries again.
		return control.Fence{}, fmt.Errorf("%w: %s", workloadproxy.ErrActivationRetry, result.Code)
	default:
		return control.Fence{}, fmt.Errorf("demand activation refused: %s", result.Code)
	}
	if result.LeaseFence == nil {
		return control.Fence{}, errors.New("demand activation returned no lease fence")
	}
	epoch, err := strconv.ParseUint(result.LeaseFence.Epoch, 10, 64)
	if err != nil {
		return control.Fence{}, err
	}
	fence := control.Fence{Incarnation: result.LeaseFence.Incarnation, Epoch: epoch}
	return fence, fence.Validate()
}
