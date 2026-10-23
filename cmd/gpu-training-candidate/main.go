// gpu-training-candidate prepares unqualified local training inputs without
// importing or executing a trainer runtime.
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
	flags := flag.NewFlagSet("gpu-training-candidate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	recipe := flags.String("recipe", "", "absolute local candidate recipe file")
	job := flags.String("job-file", "", "absolute local supervisor job envelope")
	materialize := flags.String("materialize", "", "absolute local recipe materialization input")
	output := flags.String("output", "", "new private candidate plan file")
	version := flags.Bool("version", false, "print release")
	if flags.Parse(args) != nil || flags.NArg() != 0 {
		return errors.New("usage: gpu-training-candidate (-recipe /path/recipe.json | -job-file /path/job.json | -materialize /path/inputs.json) -output /path/new-file.json")
	}
	if *version {
		if *recipe != "" || *job != "" || *materialize != "" || *output != "" {
			return errors.New("version cannot be combined with preparation flags")
		}
		_, err := fmt.Fprintln(stdout, deployment.Release)
		return err
	}
	selected := 0
	for _, input := range []string{*recipe, *job, *materialize} {
		if input != "" {
			selected++
		}
	}
	if selected != 1 || *output == "" {
		return errors.New("exactly one recipe, job-file or materialize input and output are required")
	}
	if *materialize != "" {
		return llmtraining.Materialize(*materialize, *output)
	}
	if *job != "" {
		return llmtraining.PrepareJob(*job, *output, os.Getenv("INVOCATION_ID"))
	}
	return llmtraining.Prepare(*recipe, *output)
}
