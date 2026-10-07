package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/setup"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
)

const cmdVerifyBindings = "verify-bindings"

type setupActions struct {
	home       func() (string, error)
	apply      func(context.Context, string, setup.Request) error
	reconcile  func(context.Context, string) error
	policyTick func(context.Context, string) error
	discover   func(context.Context, string) (setup.Discovery, error)
	probe      func(context.Context, setup.ProbeRequest) (setup.ApplicationCandidate, error)
	euid       func() int
	verify     func(context.Context, setup.Request) error
	inspect    func(string, control.NativeModel) (string, error)
}

func systemActions() setupActions {
	return setupActions{
		home:       setup.Home,
		apply:      setup.Apply,
		reconcile:  setup.Reconcile,
		policyTick: setup.PolicyTick,
		discover:   setup.Discover,
		probe:      setup.Probe,
		euid:       os.Geteuid,
		verify:     setup.VerifyBindings,
		inspect:    runtime.InspectQualifiedNativeLaunch,
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
		return errors.New("usage: gpu-setup discover|probe|fingerprint|drafts|save-drafts|verify-bindings|validate|apply|reconcile|idle-policy-tick|remove-integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	home, err := a.home()
	if err != nil {
		return err
	}
	switch args[0] {
	case "discover", "fingerprint", "probe", "reconcile", "idle-policy-tick", "save-drafts", cmdVerifyBindings, "validate", "apply":
		if a.euid() == 0 {
			return errors.New("run guided setup as the desktop account, not root")
		}
	}
	switch args[0] {
	case "remove-integration":
		return setup.RemoveIntegration(home)
	case "discover":
		return a.runDiscover(ctx, home, output)
	case "drafts":
		return runDrafts(home, output)
	case "save-drafts":
		return runSaveDrafts(home, input, output)
	case "fingerprint":
		return a.fingerprint(input, output)
	case "probe":
		return a.runProbe(ctx, input, output)
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
