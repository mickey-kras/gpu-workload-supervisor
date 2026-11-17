package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
)

func TestImportCLIRejectsOverridesAndUnsupportedTargetsPrivately(t *testing.T) {
	for _, args := range [][]string{nil, {"-activate", "SECRET"}, {"-recipe", "SECRET", "extra"}, {"-recipe", "SECRET", "-format", "vllm", "-output", "/private/plan"}, {"-recipe", "/missing/recipe", "-format", "peft", "-output", "/missing/plan"}} {
		var stdout, stderr bytes.Buffer
		if exitCode(args, &stdout, &stderr) != 1 || stdout.Len() != 0 || stderr.Len() == 0 || strings.Contains(stderr.String(), "SECRET") {
			t.Fatal("unexpected import diagnostic", stderr.String())
		}
	}
}

func TestImportVersionDoesNotPrepare(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if exitCode([]string{"-version"}, &stdout, &stderr) != 0 || stdout.String() != deployment.Release+"\n" || stderr.Len() != 0 {
		t.Fatal("version did not report release")
	}
	for _, name := range []string{"-recipe", "-output", "-format"} {
		stderr.Reset()
		if exitCode([]string{"-version", name, "SECRET"}, io.Discard, &stderr) != 1 || strings.Contains(stderr.String(), "SECRET") {
			t.Fatal("version accepted preparation arguments")
		}
	}
	if exitCode([]string{"-version"}, failingWriter{}, io.Discard) != 1 {
		t.Fatal("ignored output failure")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("output unavailable") }
