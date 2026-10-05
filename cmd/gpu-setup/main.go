package main

import (
	"bytes"
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
)

type setupActions struct {
	home      func() (string, error)
	apply     func(context.Context, string, setup.Request) error
	reconcile func(context.Context, string) error
	discover  func(context.Context, string) (setup.Discovery, error)
	probe     func(context.Context, setup.ProbeRequest) (setup.ApplicationCandidate, error)
	euid      func() int
	verify    func(context.Context, setup.Request) error
	inspect   func(string, control.NativeModel) (string, error)
}

func systemActions() setupActions {
	return setupActions{
		home:      setup.Home,
		apply:     setup.Apply,
		reconcile: setup.Reconcile,
		discover:  setup.Discover,
		probe:     setup.Probe,
		euid:      os.Geteuid,
		verify:    setup.VerifyBindings,
		inspect:   runtime.InspectQualifiedNativeLaunch,
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
		return errors.New("usage: gpu-setup discover|probe|fingerprint|drafts|save-drafts|verify-bindings|validate|apply|reconcile|remove-integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	home, err := a.home()
	if err != nil {
		return err
	}
	switch args[0] {
	case "discover", "fingerprint", "probe", "reconcile", "save-drafts", "verify-bindings", "validate", "apply":
		if a.euid() == 0 {
			return errors.New("run guided setup as the desktop account, not root")
		}
	}
	if args[0] == "remove-integration" {
		return setup.RemoveIntegration(home)
	}
	if args[0] == "discover" {
		result, err := a.discover(ctx, home)
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(result)
	}
	if args[0] == "drafts" {
		result, err := setup.ReadDrafts(home)
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(result)
	}
	if args[0] == "save-drafts" {
		var request setup.DraftRequest
		data, err := io.ReadAll(io.LimitReader(input, 65537))
		if err != nil {
			return err
		}
		if len(data) > 65536 {
			return errors.New("draft request exceeds 64 KiB")
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			return err
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return errors.New("trailing draft request")
		}
		result, err := setup.SaveDrafts(home, request)
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(result)
	}
	if args[0] == "fingerprint" {
		return a.fingerprint(input, output)
	}
	if args[0] == "probe" {
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
	if args[0] == "reconcile" {
		return a.reconcile(ctx, home)
	}
	if args[0] != "validate" && args[0] != "apply" && args[0] != "verify-bindings" {
		return errors.New("unknown setup action")
	}
	request, err := setup.Decode(input)
	if err != nil {
		return err
	}
	preview, err := setup.Plan(home, request)
	if err != nil {
		return err
	}
	if args[0] == "verify-bindings" {
		if err := a.verify(ctx, request); err != nil {
			return err
		}
	}
	if args[0] == "apply" {
		if err := a.apply(ctx, home, request); err != nil {
			return err
		}
	}
	return json.NewEncoder(output).Encode(preview)
}
