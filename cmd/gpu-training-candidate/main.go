// gpu-training-candidate prepares unqualified local training inputs without
// importing or executing a trainer runtime.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/llmtraining"
)

func main() {
	os.Exit(exitCode(os.Args[1:], os.Stderr))
}

func exitCode(args []string, stderr io.Writer) int {
	if err := run(args); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func run(args []string) error {
	flags := flag.NewFlagSet("gpu-training-candidate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	recipe := flags.String("recipe", "", "absolute local candidate recipe file")
	job := flags.String("job-file", "", "absolute local supervisor job envelope")
	output := flags.String("output", "", "new private candidate plan file")
	if flags.Parse(args) != nil || flags.NArg() != 0 || (*recipe == "") == (*job == "") || *output == "" {
		return errors.New("usage: gpu-training-candidate (-recipe /path/recipe.json | -job-file /path/job.json) -output /path/new-plan.json")
	}
	if *job != "" {
		return llmtraining.PrepareJob(*job, *output, os.Getenv("INVOCATION_ID"))
	}
	return llmtraining.Prepare(*recipe, *output)
}
