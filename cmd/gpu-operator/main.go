package main

import (
	"github.com/mickey-kras/gpu-workload-supervisor/internal/operator"
	"os"
)

func main() {
	if operator.Serve(os.Args[1:], os.Stdin, os.Stdout, operator.LoadProfile, operator.DetachSession) != nil {
		os.Exit(1)
	}
}
