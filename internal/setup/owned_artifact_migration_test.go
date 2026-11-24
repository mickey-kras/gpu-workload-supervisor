package setup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/llm-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/llm-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/llm-workload-supervisor/internal/supervisor"
)

func TestSetupReplacesOrRemovesPreSandboxOwnedUnit(t *testing.T) {
	for _, action := range []string{"replace", "remove", "busy"} {
		t.Run(action, func(t *testing.T) {
			ctx := t.Context()
			backend, home, request := fixture(t)
			backend.runCommand = fakeOwnedCommandFor(home)
			current, _ := ownedFixtureProfile(t, home, "vision", 9100)
			request.Catalog = control.Catalog{Version: control.CatalogVersionArtifactSandbox, Profiles: []control.WorkloadProfile{current}}
			if err := backend.Apply(ctx, home, request); err != nil {
				t.Fatal(err)
			}
			prior := acceptedRevision(t, request)
			legacy, oldBytes := preSandboxOwnedFixtureProfile(t, home, "vision", 9100)
			catalog := control.Catalog{Version: control.CatalogVersionOwned, Profiles: []control.WorkloadProfile{legacy}}
			s := openStoreAt(t, request.Profile.StatePath)
			old, err := s.ReplaceCatalog(ctx, prior.Revision, catalog)
			s.Close()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(legacy.NativeModel.LaunchFile, oldBytes, 0600); err != nil {
				t.Fatal(err)
			}
			executable := filepath.Join(home, "never-run")
			if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
				t.Fatal(err)
			}
			strict, err := gpuruntime.NewSystemdManager(gpuruntime.SystemdConfig{
				Catalog: &catalog, SystemctlPath: executable, NvidiaSMIPath: executable, HealthTimeout: time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := strict.Start(ctx, legacy.ID); !errors.Is(err, gpuruntime.ErrLaunchUnsupported) {
				t.Fatalf("runtime admitted pre-sandbox binding: %v", err)
			}

			proofs := 0
			busy := errors.New("legacy workload remains active")
			backend.makeRuntime = func(got Request) (supervisor.Runtime, error) {
				for _, p := range got.Catalog.Profiles {
					if legacyArtifactBinding(p) {
						return nil, gpuruntime.ErrLaunchUnsupported
					}
				}
				if got.Catalog.Version != control.CatalogVersionOwned {
					return idleRuntime{}, nil
				}
				manager := launchPreflightRuntime{
					verify: func() error { return errors.New("legacy unit proof missing") },
					preview: func(removals map[string]string) error {
						raw, err := os.ReadFile(legacy.NativeModel.LaunchFile)
						if err != nil || digest(raw) != removals[legacy.Unit] || digest(raw) != legacy.NativeModel.LaunchSHA256 {
							return errors.New("legacy unit proof mismatch")
						}
						proofs++
						return nil
					},
				}
				if action == "busy" {
					manager.err = busy
				}
				return manager, nil
			}
			request.ExpectedRevision = old.Revision
			request.Catalog = catalog
			if action == "remove" {
				request.Catalog = control.Catalog{Version: control.CatalogVersionLegacy, Disabled: true}
			}
			err = backend.Apply(ctx, home, request)
			accepted := acceptedRevision(t, request)
			if action == "busy" {
				if !errors.Is(err, busy) || accepted.Revision != old.Revision {
					t.Fatalf("active old unit was replaced: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			} else if action == "replace" && (accepted.Catalog.Version != control.CatalogVersionArtifactSandbox || !accepted.Catalog.Profiles[0].HasArtifactSandbox()) {
				t.Fatal("replacement did not commit the sandboxed catalog")
			} else if action == "remove" {
				if _, err := os.Stat(legacy.NativeModel.LaunchFile); !accepted.Catalog.Disabled || !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("legacy unit survived removal: %v", err)
				}
			}
			if proofs == 0 {
				t.Fatal("setup did not prove the accepted legacy bytes")
			}
		})
	}
}

// A stripped legacy binding has no GPU identity. Setup must still observe it
// on a deployment pinned to a GPU UUID, without allowing execution.
func TestLegacyObservationKeepsGPUPinnedRuntime(t *testing.T) {
	_, home, request := fixture(t)
	const uuid = "GPU-01234567-89ab-cdef-0123-456789abcdef"
	legacy, _ := preSandboxOwnedFixtureProfile(t, home, "vision", 9100)
	legacy.NativeModel.GPUUUID = uuid
	request.Profile.GPUUUID = uuid
	executable := filepath.Join(home, "never-run")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	request.Profile.SystemctlPath, request.Profile.NvidiaSMIPath = executable, executable
	request.Catalog = removalObservationCatalog(control.Catalog{Version: control.CatalogVersionOwned, Profiles: []control.WorkloadProfile{legacy}})
	if _, err := runtimeFor(request); err == nil || !strings.Contains(err.Error(), "GPU UUID") {
		t.Fatalf("an execution runtime accepted a workload without GPU identity: %v", err)
	}
	request.observation = true
	manager, err := runtimeFor(request)
	if err != nil {
		t.Fatalf("observation runtime = %v", err)
	}
	if err := manager.Start(t.Context(), legacy.ID); err == nil {
		t.Fatal("observation runtime started a workload")
	}
}
