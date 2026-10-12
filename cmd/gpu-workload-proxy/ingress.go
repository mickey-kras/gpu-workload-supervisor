package main

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	workloadproxy "github.com/mickey-kras/gpu-workload-supervisor/internal/proxy"
)

func configureIngress(config *workloadproxy.Config, mode, executionFile, completionFile string) error {
	config.IngressMode = mode
	switch mode {
	case workloadproxy.IngressTrusted:
		if executionFile != "" || completionFile != "" {
			return errors.New("credential files require adapter ingress mode")
		}
		return nil
	case workloadproxy.IngressAdapter:
		if executionFile == "" || completionFile == "" {
			return errors.New("adapter ingress requires execution-credential-file and completion-credential-file")
		}
	default:
		return errors.New("ingress-mode must be trusted or adapter")
	}
	var err error
	config.ExecutionCredential, err = readIngressCredential(executionFile)
	if err != nil {
		return fmt.Errorf("execution credential file: %w", err)
	}
	config.CompletionCredential, err = readIngressCredential(completionFile)
	if err != nil {
		return fmt.Errorf("completion credential file: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(config.ExecutionCredential), []byte(config.CompletionCredential)) == 1 {
		return errors.New("execution and completion credentials must be distinct")
	}
	return nil
}

func readIngressCredential(path string) (string, error) {
	file, err := deployment.OpenPrivate(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	// At most 64 hex characters and one final newline. The extra byte detects
	// oversized files without reading unbounded input or exposing its contents.
	data, err := io.ReadAll(io.LimitReader(file, 66))
	if err != nil {
		return "", err
	}
	value := strings.TrimSuffix(string(data), "\n")
	if !workloadproxy.ValidIngressCredential(value) {
		return "", errors.New("credential must contain 64 lowercase hexadecimal characters, optionally followed by one newline")
	}
	return value, nil
}
