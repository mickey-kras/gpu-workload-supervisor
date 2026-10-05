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

	"github.com/mickey-kras/gpu-workload-supervisor/internal/setup"
)

var homeForSetup = setup.Home
var applySetup = setup.Apply
var reconcileSetup = setup.Reconcile
var discoverSetup = setup.Discover
var probeSetup = setup.Probe
var effectiveUID = os.Geteuid
var verifyBindings = setup.VerifyBindings

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(args []string, input io.Reader, output io.Writer) error {
	if len(args) != 1 {
		return errors.New("usage: gpu-setup discover|probe|fingerprint|drafts|save-drafts|verify-bindings|validate|apply|reconcile|remove-integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	home, err := homeForSetup()
	if err != nil {
		return err
	}
	if args[0] == "remove-integration" {
		return setup.RemoveIntegration(home)
	}
	if args[0] == "discover" {
		result, err := discoverSetup(ctx, home)
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
		if effectiveUID() == 0 {
			return errors.New("run guided setup as the desktop account, not root")
		}
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
		return fingerprint(input, output)
	}
	if args[0] == "probe" {
		request, err := setup.DecodeProbe(input)
		if err != nil {
			return err
		}
		result, err := probeSetup(ctx, request)

		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(result)
	}
	if args[0] == "reconcile" {
		return reconcileSetup(ctx, home)
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
		if err := verifyBindings(ctx, request); err != nil {
			return err
		}
	}
	if args[0] == "apply" {
		if err := applyAsDesktopAccount(ctx, home, request); err != nil {
			return err
		}
	}
	return json.NewEncoder(output).Encode(preview)
}

func applyAsDesktopAccount(ctx context.Context, home string, request setup.Request) error {
	if effectiveUID() == 0 {
		return errors.New("run guided setup as the desktop account, not root")
	}
	return applySetup(ctx, home, request)
}
