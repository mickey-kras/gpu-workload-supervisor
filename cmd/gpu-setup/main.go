package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/setup"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
)

const (
	cmdVerifyBindings = "verify-bindings"
	cmdModelStatus    = "model-status"
	cmdModelCheck     = "model-check"
	cmdModelReview    = "model-review"
	cmdModelApprove   = "model-approve"
)

type setupActions struct {
	home          func() (string, error)
	apply         func(context.Context, string, setup.Request) error
	reconcile     func(context.Context, string) error
	policyTick    func(context.Context, string) error
	discover      func(context.Context, string) (setup.Discovery, error)
	prepare       func(context.Context, setup.PrepareRequest) (setup.PreparedApplication, error)
	probe         func(context.Context, setup.ProbeRequest) (setup.ApplicationCandidate, error)
	euid          func() int
	verify        func(context.Context, setup.Request) error
	inspect       func(string, control.NativeModel) (control.NativeModel, string, error)
	managerCgroup func(context.Context, string) (string, error)
}

func systemActions() setupActions {
	backend := setup.SystemBackend()
	return setupActions{
		home:          setup.Home,
		apply:         backend.Apply,
		reconcile:     backend.Reconcile,
		policyTick:    setup.PolicyTick,
		discover:      backend.Discover,
		probe:         setup.Probe,
		prepare:       backend.Prepare,
		euid:          os.Geteuid,
		verify:        backend.VerifyBindings,
		inspect:       runtime.BindHardenedNativeLaunch,
		managerCgroup: backend.ManagerCgroup,
	}
}

func main() {
	if err := systemActions().run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func (a setupActions) run(args []string, input io.Reader, output io.Writer) error {
	if len(args) == 1 && (args[0] == "-version" || args[0] == "--version") {
		_, err := fmt.Fprintln(output, deployment.Release)
		return err
	}
	if len(args) != 1 {
		return errors.New("usage: gpu-setup -version|temporary-status|temporary-cleanup|discover|probe|prepare|correct-gpu|fingerprint|render-owned|drafts|save-drafts|verify-bindings|validate|apply|reconcile|idle-policy-tick|remove-integration|model-status|model-check|model-review|model-approve")
	}
	ctx, cancel := actionContext(args[0])
	defer cancel()
	home, err := a.home()
	if err != nil {
		return err
	}
	switch args[0] {
	case "temporary-status", "temporary-cleanup", "correct-gpu", "discover", "fingerprint", "probe", "prepare", "reconcile", "idle-policy-tick", "drafts", "save-drafts", "remove-integration", cmdVerifyBindings, "validate", "apply", "render-owned", cmdModelStatus, cmdModelCheck, cmdModelReview, cmdModelApprove:
		if a.euid() == 0 {
			return errors.New("run guided setup as the desktop account, not root")
		}
	}
	return a.runAction(ctx, home, args[0], input, output)
}

func (a setupActions) runAction(ctx context.Context, home, action string, input io.Reader, output io.Writer) error {
	switch action {
	case "temporary-status":
		result, err := setup.TemporaryStatus(ctx, home)
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(result)
	case "temporary-cleanup":
		var request setup.TemporaryCleanupRequest
		if err := strictjson.DecodeLimited(input, 65536, &request); err != nil {
			return err
		}
		return setup.SystemBackend().TemporaryCleanup(ctx, home, request)
	case "correct-gpu":
		return runGPUCorrection(ctx, home, input, output)
	case "remove-integration":
		return setup.SystemBackend().RemoveIntegration(ctx, home)
	case "discover":
		return a.runDiscover(ctx, home, output)
	case cmdModelStatus:
		var request setup.ModelStatusRequest
		if err := strictjson.DecodeLimited(input, setup.MaxModelStatusRequestBytes, &request); err != nil {
			return err
		}
		ctx, cancel := modelStatusContext(ctx, request)
		defer cancel()
		report, err := setup.ModelStatus(ctx, home, request)
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(report)
	case cmdModelCheck, cmdModelReview, cmdModelApprove:
		return runModelAction(ctx, home, action, input, output)
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
		return a.runPlanned(ctx, home, action, input, output)
	}
	return errors.New("unknown setup action")
}

func runGPUCorrection(ctx context.Context, home string, input io.Reader, output io.Writer) error {
	var request setup.GPUCorrectionRequest
	if err := strictjson.DecodeLimited(input, 16384, &request); err != nil {
		return err
	}
	result, err := setup.SystemBackend().CorrectGPU(ctx, home, request)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(result)
}

func (a setupActions) runDiscover(ctx context.Context, home string, output io.Writer) error {
	result, err := a.discover(ctx)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(result)
}