package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/setup"
)

func TestRunProbe(t *testing.T) {
	actions := systemActions()
	actions.home = func() (string, error) { return t.TempDir(), nil }
	actions.euid = func() int { return 1000 }
	for _, failed := range []bool{false, true} {
		actions.probe = func(ctx context.Context, r setup.ProbeRequest) (setup.ApplicationCandidate, error) {
			if r.App != "ollama" {
				t.Fatal(r)
			}
			if failed {
				return setup.ApplicationCandidate{}, errors.New("cancelled")
			}
			return setup.ApplicationCandidate{App: "ollama", LifecycleControl: "unverified"}, nil
		}
		var output bytes.Buffer
		err := actions.run([]string{"probe"}, strings.NewReader(`{"app":"ollama","endpoint":"http://127.0.0.1"}`), &output)
		if (err != nil) != failed {
			t.Fatal(err)
		}
		if !failed && !strings.Contains(output.String(), `"lifecycleControl":"unverified"`) {
			t.Fatal(output.String())
		}
	}
	if err := actions.run([]string{"probe"}, strings.NewReader(`{}`), &bytes.Buffer{}); err == nil {
		t.Fatal("bad probe accepted")
	}
}
