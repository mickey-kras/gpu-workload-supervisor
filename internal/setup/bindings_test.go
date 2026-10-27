package setup

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
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

func adoptedProfiles() []control.WorkloadProfile {
	adopted := func(id, model, port string) control.WorkloadProfile {
		endpoint := "http://127.0.0.1:" + port
		return control.WorkloadProfile{ID: control.Workload(id), Label: id, Adapter: "systemd", Unit: "model-" + id + ".service", Cgroup: "/user.slice/model-" + id + ".service", HealthURL: endpoint + "/health", NativeModel: &control.NativeModel{Runtime: "vllm", Instance: id, Model: model, Endpoint: endpoint, LaunchFile: "/etc/systemd/user/model-" + id + ".service", LaunchSHA256: strings.Repeat("0", 64)}}
	}
	return []control.WorkloadProfile{adopted("alpha", "a", "8081"), adopted("beta", "b", "8082")}
}

func TestAdoptedProfilesPreserveAcceptedCatalogOnDriftOrStaleRevision(t *testing.T) {
	for _, failure := range []string{"carried binding drift", "stale revision", "precommit drift"} {
		t.Run(failure, func(t *testing.T) {
			backend, home, request := fixture(t)
			request.Catalog.Profiles = adoptedProfiles()
			if err := backend.Apply(t.Context(), home, request); err != nil {
				t.Fatal(err)
			}
			accepted, err := ReadCatalog(t.Context(), request.Profile.StatePath)
			if err != nil {
				t.Fatal(err)
			}
			request.ExpectedRevision = accepted.Revision
			checks := 0
			precommitChecked := false
			backend.makeRuntime = func(got Request) (gpuruntime.Manager, error) {
				return launchPreflightRuntime{verify: func() error {
					checks++
					marker, err := deployment.Read(request.Profile.StatePath)
					precommitChecked = err == nil && marker.Maintenance && reflect.DeepEqual(got.Catalog, request.Catalog)
					if failure == "carried binding drift" || precommitChecked {
						return gpuruntime.ErrLaunchChanged
					}
					return nil
				}}, nil
			}
			if failure != "carried binding drift" {
				request.Catalog.Profiles[1].Label = "Edited"
			}
			if failure == "stale revision" {
				request.ExpectedRevision = "stale"
			}
			if err := backend.Apply(t.Context(), home, request); err == nil {
				t.Fatal("unsafe adopted catalog accepted")
			}
			snapshot, err := ReadCatalog(t.Context(), request.Profile.StatePath)
			if err != nil || !reflect.DeepEqual(snapshot, accepted) {
				t.Fatalf("accepted catalog changed: %v %v", snapshot, err)
			}
			if failure == "carried binding drift" && checks != 1 {
				t.Fatalf("carried bindings skipped final preflight: %d", checks)
			}
			if failure == "stale revision" && checks != 0 {
				t.Fatalf("stale revision reached runtime: %d", checks)
			}
			if failure == "precommit drift" && !precommitChecked {
				t.Fatal("drift was not injected at the precommit check")
			}
		})
	}
}
