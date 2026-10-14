package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestOperatorProcessHelper(t *testing.T) {
	if os.Getenv("GPU_OPERATOR_PROCESS_HELPER") != "1" {
		return
	}
	os.Args = []string{"gpu-operator"}
	main()
	os.Exit(0)
}

func operatorProcess(t *testing.T) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestOperatorProcessHelper$")
	cmd.Env = append(os.Environ(), "GPU_OPERATOR_PROCESS_HELPER=1")
	cmd.Stdin = strings.NewReader(`{"protocolVersion":2,"requestId":"r-1","action":"status"}` + "\n")
	return cmd
}

func TestOperatorDeliveredApplicationErrorExitsZero(t *testing.T) {
	cmd := operatorProcess(t)
	body, err := cmd.Output()
	var response struct {
		ProtocolVersion int    `json:"protocolVersion"`
		RequestID       string `json:"requestId"`
		Code            string `json:"code"`
	}
	if err != nil || json.Unmarshal(body, &response) != nil || response.ProtocolVersion != 1 || response.RequestID != "r-1" || response.Code != "unsupported_version" {
		t.Fatalf("process response = %q, %v", body, err)
	}
}

func TestOperatorUndeliveredResponseExitsNonzero(t *testing.T) {
	for _, mode := range []string{"broken pipe", "write timeout"} {
		t.Run(mode, func(t *testing.T) {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			defer w.Close()
			if mode == "broken pipe" {
				r.Close()
			} else {
				capacity, err := unix.FcntlInt(w.Fd(), unix.F_GETPIPE_SZ, 0)
				if err != nil {
					t.Fatal(err)
				}
				if n, err := w.Write(bytes.Repeat([]byte("x"), capacity)); err != nil || n != capacity {
					t.Fatalf("fill stdout pipe: wrote %d of %d bytes, %v", n, capacity, err)
				}
			}
			cmd := operatorProcess(t)
			cmd.Stdout = w
			start := time.Now()
			err = cmd.Run()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() == 0 || cmd.ProcessState == nil {
				t.Fatalf("undelivered response exit = %v", err)
			}
			if mode == "write timeout" && (exit.ExitCode() != 1 || time.Since(start) < 2*time.Second) {
				t.Fatalf("write deadline did not produce exit 1: %v", err)
			}
		})
	}
}
