package main

import (
	"fmt"
	"os"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/operator"
)

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "-version" || os.Args[1] == "--version") {
		if _, err := fmt.Fprintln(os.Stdout, deployment.Release); err != nil {
			os.Exit(1)
		}
		return
	}
	if operator.Serve(os.Args[1:], os.Stdin, os.Stdout, operator.LoadProfile, operator.DetachSession) != nil {
		os.Exit(1)
	}
}
