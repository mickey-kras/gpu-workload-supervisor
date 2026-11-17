package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/llmtraining"
)

func main() {
	os.Exit(exitCode(os.Args[1:], os.Stdout, os.Stderr))
}

func exitCode(args []string, stdout, stderr io.Writer) int {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	if err := run(ctx, args, stdout); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func run(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("gpu-training-runner", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	job := flags.String("job-file", "", "absolute supervisor job envelope")
	version := flags.Bool("version", false, "print release")
	if flags.Parse(args) != nil || flags.NArg() != 0 || (*job == "") == !*version {
		return errors.New("usage: gpu-training-runner --job-file /private/training/job.json")
	}
	if *version {
		_, err := fmt.Fprintln(output, deployment.Release)
		return err
	}
	return llmtraining.RunJob(ctx, *job, os.Getenv("INVOCATION_ID"), os.Getenv("CUDA_VISIBLE_DEVICES"))
}
