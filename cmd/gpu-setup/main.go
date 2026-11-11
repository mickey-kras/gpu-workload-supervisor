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

const cmdVerifyBindings = "verify-bindings"

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
	case "temporary-status", "temporary-cleanup", "correct-gpu", "discover", "fingerprint", "probe", "prepare", "reconcile", "idle-policy-tick", "drafts", "save-drafts", "remove-integration", cmdVerifyBindings, "validate", "apply", "render-owned", "model-status", "model-check", "model-review", "model-approve":
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
	case "model-status":
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
	case "model-check", "model-review", "model-approve":
		return runModelCheck(ctx, home, action, input, output)
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
	result, err := a.discover(ctx, home)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(result)
}
func runDrafts(home string, output io.Writer) error {
	result, err := setup.ReadDrafts(home)
	if err != nil && !errors.Is(err, setup.ErrUnsupportedDraftApplication) {
		return err
	}
	warning := ""
	if err != nil {
		warning = err.Error()
	}
	return json.NewEncoder(output).Encode(struct {
		setup.DraftSnapshot
		Warning string `json:"warning,omitempty"`
	}{result, warning})
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
	preview, err := setup.SystemBackend().Plan(home, request)
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

// The timeouts of the actions. Only a model status request that names
// workloads to recheck reads model files, so only it gets recheckTimeout.
// A model check reads all model files and the hub listing, so it gets
// modelCheckTimeout.
const (
	actionTimeout     = 5 * time.Minute
	recheckTimeout    = 30 * time.Minute
	modelCheckTimeout = 60 * time.Minute
)

// actionContext gives the context of an action, with its timeout. A
// recheck reads every model file again, which takes longer. A status read
// without a recheck gets actionTimeout, see modelStatusContext. An
// interrupt or a stop signal cancels a model check, which then stores
// nothing. A killed run stores nothing too: the store writes the result in
// one transaction.
func actionContext(action string) (context.Context, context.CancelFunc) {
	switch action {
	case "model-status":
		return context.WithTimeout(context.Background(), recheckTimeout)
	case "model-check":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		ctx, cancel := context.WithTimeout(ctx, modelCheckTimeout)
		return ctx, func() { cancel(); stop() }
	}
	return context.WithTimeout(context.Background(), actionTimeout)
}

// runModelCheck runs the model check, review and approval actions. Each
// writes one JSON object.
func runModelCheck(ctx context.Context, home, action string, input io.Reader, output io.Writer) error {
	var result any
	switch action {
	case "model-check":
		request, err := setup.DecodeModelCheckRequest(input)
		if err != nil {
			return err
		}
		if result, err = setup.ModelCheck(ctx, home, request, &http.Client{}); err != nil {
			return err
		}
	case "model-review":
		request, err := setup.DecodeModelReviewRequest(input)
		if err != nil {
			return err
		}
		if result, err = setup.ModelReviewList(ctx, home, request); err != nil {
			return err
		}
	default:
		request, err := setup.DecodeModelApproveRequest(input)
		if err != nil {
			return err
		}
		if result, err = setup.ModelApprove(ctx, home, request, os.Getuid(), time.Now()); err != nil {
			return err
		}
	}
	return json.NewEncoder(output).Encode(result)
}

// modelStatusContext limits a model status request without a recheck to
// actionTimeout.
func modelStatusContext(ctx context.Context, request setup.ModelStatusRequest) (context.Context, context.CancelFunc) {
	if len(request.Recheck) == 0 {
		return context.WithTimeout(ctx, actionTimeout)
	}
	return context.WithCancel(ctx)
}
