// gpu-training-import validates a local PEFT artifact without enabling serving.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/llmtraining"
)

func main() {
	os.Exit(exitCode(os.Args[1:], os.Stdout, os.Stderr))
}

func exitCode(args []string, stdout, stderr io.Writer) int {
	if err := run(args, stdout); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func run(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("gpu-training-import", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	recipe := flags.String("recipe", "", "original absolute local recipe file")
	output := flags.String("output", "", "new private import preparation plan")
	format := flags.String("format", "", "artifact format: peft")
	version := flags.Bool("version", false, "print release")
	if flags.Parse(args) != nil || flags.NArg() != 0 {
		return errors.New("usage: gpu-training-import -format peft -recipe /path/recipe.json -output /path/new-plan.json")
	}
	if *version {
		if *recipe != "" || *output != "" || *format != "" {
			return errors.New("version cannot be combined with preparation flags")
		}
		_, err := fmt.Fprintln(stdout, deployment.Release)
		return err
	}
	return llmtraining.PrepareImport(*recipe, *output, *format)
}
