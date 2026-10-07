package setup

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
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
	backend.makeRuntime = func(Request) (gpuruntime.Manager, error) { return nil, errors.New("runtime missing") }
	if err := backend.VerifyBindings(context.Background(), request); err == nil {
		t.Fatal("missing runtime")
	}
}

func TestSetupRejectsSharedOllamaUnitCatalogs(t *testing.T) {
	backend, _, request := fixture(t)
	native := func(model string) *control.NativeModel {
		return &control.NativeModel{Runtime: "ollama", Instance: "local", Model: model, Endpoint: "http://127.0.0.1:11434", LaunchFile: "/etc/systemd/user/ollama.service", LaunchSHA256: strings.Repeat("0", 64)}
	}
	shared := func(id, model string) control.WorkloadProfile {
		return control.WorkloadProfile{ID: control.Workload(id), Label: id, Adapter: "systemd", Unit: "ollama.service", Cgroup: "/user.slice/ollama.service", HealthURL: "http://127.0.0.1:11434/health", NativeModel: native(model)}
	}
	request.Catalog.Profiles = []control.WorkloadProfile{shared("alpha", "a"), shared("beta", "b")}
	if err := request.Catalog.Validate(); err != nil {
		t.Fatal("shared-unit catalog is valid for the CLI", err)
	}
	if err := Validate(request); err == nil || !strings.Contains(err.Error(), "catalog-only") {
		t.Fatalf("shared-unit catalog accepted by setup: %v", err)
	}
	if err := backend.VerifyBindings(context.Background(), request); err == nil {
		t.Fatal("verify-bindings accepted a shared-unit catalog")
	}
}
