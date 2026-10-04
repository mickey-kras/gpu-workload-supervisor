package main

import (
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
var effectiveUID = os.Geteuid

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(args []string, input io.Reader, output io.Writer) error {
	if len(args) != 1 {
		return errors.New("usage: gpu-setup discover|validate|apply|reconcile|remove-integration")
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
	if args[0] == "reconcile" {
		return reconcileSetup(ctx, home)
	}
	if args[0] != "validate" && args[0] != "apply" {
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
	if args[0] == "apply" {
		if effectiveUID() == 0 {
			return errors.New("run guided setup as the desktop account, not root")
		}
		if err := applySetup(ctx, home, request); err != nil {
			return err
		}
	}
	return json.NewEncoder(output).Encode(preview)
}
