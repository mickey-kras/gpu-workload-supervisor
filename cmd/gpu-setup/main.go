package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/setup"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
)

const cmdVerifyBindings = "verify-bindings"

type setupActions struct {
	home              func() (string, error)
	temporaryStatus   func(context.Context, string) (setup.TemporaryDiscoveryStatus, error)
	temporaryDiscover func(context.Context, string, setup.TemporaryDiscoveryRequest) (setup.TemporaryDiscoveryResult, error)
	temporaryCleanup  func(context.Context, string, setup.TemporaryCleanupRequest) (setup.TemporaryDiscoveryResult, error)
	apply             func(context.Context, string, setup.Request) error
	reconcile         func(context.Context, string) error
	policyTick        func(context.Context, string) error
	discover          func(context.Context, string) (setup.Discovery, error)
	prepare           func(context.Context, setup.PrepareRequest) (setup.PreparedApplication, error)
	probe             func(context.Context, setup.ProbeRequest) (setup.ApplicationCandidate, error)
	euid              func() int
	verify            func(context.Context, setup.Request) error
	inspect           func(string, control.NativeModel) (string, error)
	managerCgroup     func(context.Context, string) (string, error)
}

func systemActions() setupActions {
	return setupActions{
		home:              setup.Home,
		temporaryStatus:   setup.TemporaryStatus,
		temporaryDiscover: setup.TemporaryDiscover,
		temporaryCleanup:  setup.TemporaryCleanup,
		apply:             setup.Apply,
		reconcile:         setup.Reconcile,
		policyTick:        setup.PolicyTick,
		discover:          setup.Discover,
		probe:             setup.Probe,
		prepare:           setup.Prepare,
		euid:              os.Geteuid,
		verify:            setup.VerifyBindings,
		inspect:           runtime.InspectQualifiedNativeLaunch,
		managerCgroup:     setup.ManagerCgroup,
	}
}

func main() {
	if err := systemActions().run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func (a setupActions) run(args []string, input io.Reader, output io.Writer) error {
	if len(args) != 1 {
		return errors.New("usage: gpu-setup discover|probe|prepare|fingerprint|render-owned|drafts|save-drafts|verify-bindings|validate|apply|temporary-status|temporary-discover|temporary-cleanup|reconcile|idle-policy-tick|remove-integration")
	}
	parent := context.Background()
	if args[0] == "temporary-discover" {
		signalCtx, stop := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		parent = signalCtx
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	defer cancel()
	home, err := a.home()
	if err != nil {
		return err
	}
	switch args[0] {
	case "temporary-discover", "temporary-status", "temporary-cleanup", "discover", "fingerprint", "probe", "prepare", "reconcile", "idle-policy-tick", "save-drafts", cmdVerifyBindings, "validate", "apply", "render-owned":
		if a.euid() == 0 {
			return errors.New("run guided setup as the desktop account, not root")
		}
	}
	switch args[0] {
	case "temporary-status":
		result, err := a.temporaryStatus(ctx, home)
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(result)
	case "temporary-discover":
		var request setup.TemporaryDiscoveryRequest
		if err := strictjson.DecodeLimited(input, 16384, &request); err != nil {
			return err
		}
		result, err := a.temporaryDiscover(ctx, home, request)
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(result)
	case "temporary-cleanup":
		var request setup.TemporaryCleanupRequest
		if err := strictjson.DecodeLimited(input, 16384, &request); err != nil {
			return err
		}
		result, err := a.temporaryCleanup(ctx, home, request)
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(result)
	case "remove-integration":
		return setup.RemoveIntegration(ctx, home)
	case "discover":
		return a.runDiscover(ctx, home, output)
	case "drafts":
		return runDrafts(home, output)
	case "save-drafts":
		return runSaveDrafts(home, input, output)
	case "fingerprint":
		return a.fingerprint(input, output)
	case "render-owned":
		return a.renderOwned(ctx, home, input, output)
	case "probe":
		return a.runProbe(ctx, input, output)
	case "prepare":
		var request setup.PrepareRequest
		if err := strictjson.DecodeLimited(input, 65536, &request); err != nil {
			return err
		}
		result, err := a.prepare(ctx, request)
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(result)
	case "reconcile":
		return a.reconcile(ctx, home)
	case "idle-policy-tick":
		return a.policyTick(ctx, home)
	case "validate", "apply", cmdVerifyBindings:
		return a.runPlanned(ctx, home, args[0], input, output)
	}
	return errors.New("unknown setup action")
}
func (a setupActions) runDiscover(ctx context.Context, home string, output io.Writer) error {
	result, err := a.discover(ctx, home)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(result)
}
func runDrafts(home string, output io.Writer) error {
	result, err := setup.ReadDrafts(home)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(result)
}
func runSaveDrafts(home string, input io.Reader, output io.Writer) error {
	var request setup.DraftRequest
	if err := strictjson.DecodeLimited(input, 65536, &request); err != nil {
		return err
	}
	result, err := setup.SaveDrafts(home, request)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(result)
}
func (a setupActions) runProbe(ctx context.Context, input io.Reader, output io.Writer) error {
	request, err := setup.DecodeProbe(input)
	if err != nil {
		return err
	}
	result, err := a.probe(ctx, request)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(result)
}
func (a setupActions) runPlanned(ctx context.Context, home, action string, input io.Reader, output io.Writer) error {
	request, err := setup.Decode(input)
	if err != nil {
		return err
	}
	preview, err := setup.Plan(home, request)
	if err != nil {
		return err
	}
	if action == cmdVerifyBindings {
		if err := a.verify(ctx, request); err != nil {
			return err
		}
	}
	if action == "apply" {
		if err := a.apply(ctx, home, request); err != nil {
			return err
		}
	}
	return json.NewEncoder(output).Encode(preview)
}
