package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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

func sharedAdoptedProfiles() []control.WorkloadProfile {
	native := func(model string) *control.NativeModel {
		return &control.NativeModel{Runtime: "ollama", Instance: "local", Model: model, Endpoint: "http://127.0.0.1:11434", LaunchFile: "/etc/systemd/user/ollama.service", LaunchSHA256: strings.Repeat("0", 64)}
	}
	shared := func(id, model string) control.WorkloadProfile {
		return control.WorkloadProfile{ID: control.Workload(id), Label: id, Adapter: "systemd", Unit: "ollama.service", Cgroup: "/user.slice/ollama.service", HealthURL: "http://127.0.0.1:11434/health", NativeModel: native(model)}
	}
	return []control.WorkloadProfile{shared("alpha", "a"), shared("beta", "b")}
}

func TestApplyNewSharedAdoptedOllamaGroup(t *testing.T) {
	for _, failure := range []string{"none", "preflight", "release", "mismatched hash", "mismatched file", "duplicate model", "conflicting endpoint", "conflicting cgroup"} {
		t.Run(failure, func(t *testing.T) {
			backend, home, request := fixture(t)
			request.Catalog.Profiles = sharedAdoptedProfiles()
			switch failure {
			case "mismatched hash":
				request.Catalog.Profiles[1].NativeModel.LaunchSHA256 = strings.Repeat("1", 64)
			case "mismatched file":
				request.Catalog.Profiles[1].NativeModel.LaunchFile = "/etc/systemd/user/another.service"
			case "duplicate model":
				request.Catalog.Profiles[1].NativeModel.Model = "a:latest"
			case "conflicting endpoint":
				request.Catalog.Profiles[1].NativeModel.Endpoint = "http://127.0.0.1:11435"
			case "conflicting cgroup":
				request.Catalog.Profiles[1].Cgroup = "/user.slice/other.service"
			}
			checks := 0
			backend.makeRuntime = func(Request) (gpuruntime.Manager, error) {
				var releaseError error
				if failure == "release" {
					releaseError = errors.New("GPU release unavailable")
				}
				return launchPreflightRuntime{idleRuntime: idleRuntime{err: releaseError}, verify: func() error {
					checks++
					if failure == "preflight" {
						return gpuruntime.ErrLaunchChanged
					}
					return nil
				}}, nil
			}
			err := backend.Apply(t.Context(), home, request)
			if (err != nil) != (failure != "none") {
				t.Fatalf("%s: %v", failure, err)
			}
			if failure == "none" {
				snapshot, err := ReadCatalog(t.Context(), request.Profile.StatePath)
				if err != nil || !reflect.DeepEqual(snapshot.Catalog, request.Catalog) {
					t.Fatalf("group not committed: %v %v", snapshot, err)
				}
				if checks < 3 {
					t.Fatalf("read-only and precommit preflight missing: %d", checks)
				}
			} else if _, err := os.Stat(request.Profile.StatePath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid group reached catalog commit: %v", err)
			}
			if _, err := os.Stat(filepath.Join(ownedUnitDirectory(home), "ollama.service")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("adopted group wrote external unit: %v", err)
			}
		})
	}
}

func TestNewAdoptedGroupVerifiedBeforePendingOwnedWrite(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "valid", true: "changed binding"}[fail], func(t *testing.T) {
			backend, home, request := fixture(t)
			backend.runCommand = fakeOwnedCommandFor(home)
			owned, raw := ownedFixtureProfile(t, home, "vision", 9100)
			request.Catalog = control.Catalog{Version: 2, Profiles: append(sharedAdoptedProfiles(), owned)}
			path := filepath.Join(ownedUnitDirectory(home), owned.Unit)
			previews, full := 0, 0
			backend.makeRuntime = func(Request) (gpuruntime.Manager, error) {
				return launchPreflightRuntime{preview: func(map[string]string) error {
					previews++
					if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("adopted preview ran after owned write: %v", err)
					}
					if fail {
						return gpuruntime.ErrLaunchChanged
					}
					return nil
				}, verify: func() error {
					full++
					bytes, err := os.ReadFile(path)
					if err != nil || string(bytes) != string(raw) {
						t.Fatalf("full preflight missing owned unit: %v", err)
					}
					return nil
				}}, nil
			}
			err := backend.Apply(t.Context(), home, request)
			if (err != nil) != fail {
				t.Fatal(err)
			}
			if previews != 1 {
				t.Fatalf("adopted preview count: %d", previews)
			}
			if fail {
				if full != 0 {
					t.Fatal("failed adopted preview reached owned activation")
				}
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("failed adopted preview wrote owned launch: %v", err)
				}
			} else if full < 2 {
				t.Fatalf("complete preflight count: %d", full)
			}
		})
	}
}

func TestSharedAdoptedGroupPreservesAcceptedCatalogOnDriftOrStaleRevision(t *testing.T) {
	for _, failure := range []string{"carried binding drift", "stale revision", "precommit drift"} {
		t.Run(failure, func(t *testing.T) {
			backend, home, request := fixture(t)
			request.Catalog.Profiles = sharedAdoptedProfiles()
			if err := backend.Apply(t.Context(), home, request); err != nil {
				t.Fatal(err)
			}
			accepted, err := ReadCatalog(t.Context(), request.Profile.StatePath)
			if err != nil {
				t.Fatal(err)
			}
			request.ExpectedRevision = accepted.Revision
			checks := 0
			backend.makeRuntime = func(Request) (gpuruntime.Manager, error) {
				return launchPreflightRuntime{verify: func() error {
					checks++
					if failure == "carried binding drift" || checks >= 3 {
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
				t.Fatal("unsafe shared group accepted")
			}
			snapshot, err := ReadCatalog(t.Context(), request.Profile.StatePath)
			if err != nil || !reflect.DeepEqual(snapshot, accepted) {
				t.Fatalf("accepted catalog changed: %v %v", snapshot, err)
			}
			if failure == "carried binding drift" && checks != 1 {
				t.Fatalf("carried group skipped final preflight: %d", checks)
			}
			if failure == "stale revision" && checks != 0 {
				t.Fatalf("stale revision reached runtime: %d", checks)
			}
			if failure == "precommit drift" && checks != 3 {
				t.Fatalf("precommit drift checks: %d", checks)
			}
		})
	}
}
