package main

import (
	"bytes"
	"context"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/setup"
	"strings"
	"testing"
)

func TestPrepareCommandIsReadOnlyAndRejectsRoot(t *testing.T) {
	actions := systemActions()
	actions.home = func() (string, error) { return t.TempDir(), nil }
	actions.euid = func() int { return 1000 }
	actions.prepare = func(_ context.Context, r setup.PrepareRequest) (setup.PreparedApplication, error) {
		if r.Draft.App != "comfyui" {
			return setup.PreparedApplication{}, errors.New("unsupported selection")
		}
		return setup.PreparedApplication{Profile: control.WorkloadProfile{ID: "comfy", Unit: r.Draft.Binding.Unit}}, nil
	}
	input := `{"draft":{"id":"comfy","label":"ComfyUI","app":"comfyui","binding":{"unit":"comfy.service"}}}`
	var out bytes.Buffer
	if err := actions.run([]string{"prepare"}, strings.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"unit":"comfy.service"`) {
		t.Fatal(out.String())
	}
	for _, bad := range []string{`{`, `{"draft":{},"unexpected":true}`, `{"draft":{"app":"unsupported"}}`} {
		if err := actions.run([]string{"prepare"}, strings.NewReader(bad), &bytes.Buffer{}); err == nil {
			t.Fatal(bad)
		}
	}
	actions.euid = func() int { return 0 }
	if err := actions.run([]string{"prepare"}, strings.NewReader(input), &bytes.Buffer{}); err == nil {
		t.Fatal("root permitted")
	}
}
