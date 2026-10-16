package main

import (
	"context"
	"errors"
	"strconv"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/operator"
	workloadproxy "github.com/mickey-kras/gpu-workload-supervisor/internal/proxy"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
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
	if result.Code != operator.OK || result.LeaseFence == nil {
		return control.Fence{}, errors.New("demand activation deferred")
	}
	epoch, err := strconv.ParseUint(result.LeaseFence.Epoch, 10, 64)
	if err != nil {
		return control.Fence{}, err
	}
	fence := control.Fence{Incarnation: result.LeaseFence.Incarnation, Epoch: epoch}
	return fence, fence.Validate()
}
