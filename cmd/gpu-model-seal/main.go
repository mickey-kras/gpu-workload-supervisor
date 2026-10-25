// gpu-model-seal makes model attestation material and checks signed model
// attestations. It is a one-shot command for isolated staging. It never
// receives or stores a signing key, and it has no daemon or listener.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/attestation"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/modelseal"
)

const usage = `usage:
  gpu-model-seal seal (-root /model/dir | -file /model/file) (-scanner-config /path/scanner.json | -not-scanned) -policy-version N [-source-uri URI] [-source-revision REV] -output /private/new-dir
  gpu-model-seal verify (-root /model/dir | -file /model/file) -envelope /path/envelope.json -signature /path/signature.json -trust /path/trust.json [-policy /path/policy.json]
  gpu-model-seal -version`

// errRejected marks evidence that verification or policy did not accept.
var errRejected = errors.New("attestation is not accepted")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := exitCode(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func exitCode(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if err := run(ctx, args, stdout); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func run(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 1 && args[0] == "-version" {
		_, err := fmt.Fprintln(stdout, deployment.Release)
		return err
	}
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "seal":
		return seal(ctx, args[1:], stdout)
	case "verify":
		return verify(args[1:], stdout)
	}
	return errors.New(usage)
}

func target(flags *flag.FlagSet) *modelseal.Target {
	var t modelseal.Target
	flags.StringVar(&t.Root, "root", "", "absolute inventory root")
	flags.StringVar(&t.File, "file", "", "absolute single artifact file")
	return &t
}

func seal(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("seal", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	t := target(flags)
	scanner := flags.String("scanner-config", "", "absolute scanner configuration file")
	notScanned := flags.Bool("not-scanned", false, "record an intentionally skipped scan")
	policyVersion := flags.Int("policy-version", 0, "policy version that the producer applied")
	uri := flags.String("source-uri", "", "where the artifact came from")
	revision := flags.String("source-revision", "", "source revision")
	output := flags.String("output", "", "new private output directory")
	if flags.Parse(args) != nil || flags.NArg() != 0 {
		return errors.New(usage)
	}
	request := modelseal.SealRequest{Target: *t, ScannerConfig: *scanner, NotScanned: *notScanned, PolicyVersion: *policyVersion, Output: *output}
	if *uri != "" || *revision != "" {
		request.Source = &attestation.Source{URI: *uri, Revision: *revision}
	}
	sealed, err := modelseal.Seal(ctx, request)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(sealed)
}

func verify(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	t := target(flags)
	var request modelseal.VerifyRequest
	flags.StringVar(&request.Envelope, "envelope", "", "envelope file")
	flags.StringVar(&request.Signature, "signature", "", "detached signature file")
	flags.StringVar(&request.Trust, "trust", "", "trust configuration with pinned public keys")
	flags.StringVar(&request.Policy, "policy", "", "optional policy file")
	if flags.Parse(args) != nil || flags.NArg() != 0 || request.Envelope == "" || request.Signature == "" || request.Trust == "" {
		return errors.New(usage)
	}
	request.Target = *t
	outcome, err := modelseal.Verify(request)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(stdout).Encode(outcome); err != nil {
		return err
	}
	if !outcome.Accepted() {
		return errRejected
	}
	return nil
}
