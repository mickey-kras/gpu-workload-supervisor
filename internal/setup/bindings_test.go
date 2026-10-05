package setup

import (
	"context"
	"errors"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"testing"
)

type bindingRuntime struct {
	idleRuntime
	preflightError error
}

func (r bindingRuntime) Preflight(context.Context) error { return r.preflightError }
func TestVerifyBindingsFailsClosed(t *testing.T) {
	backend, _, request := fixture(t)
	if err := backend.VerifyBindings(context.Background(), Request{}); err == nil {
		t.Fatal("invalid request")
	}
	for _, fail := range []bool{false, true} {
		backend.makeRuntime = func(Request) (gpuruntime.Manager, error) {
			var err error
			if fail {
				err = errors.New("binding changed")
			}
			return bindingRuntime{preflightError: err}, nil
		}
		if err := backend.VerifyBindings(context.Background(), request); (err != nil) != fail {
			t.Fatalf("failure=%v: %v", fail, err)
		}
	}
	backend.makeRuntime = func(Request) (gpuruntime.Manager, error) { return idleRuntime{}, nil }
	if err := backend.VerifyBindings(context.Background(), request); err == nil {
		t.Fatal("missing verifier")
	}
	backend.makeRuntime = func(Request) (gpuruntime.Manager, error) { return nil, errors.New("runtime missing") }
	if err := backend.VerifyBindings(context.Background(), request); err == nil {
		t.Fatal("missing runtime")
	}
}
