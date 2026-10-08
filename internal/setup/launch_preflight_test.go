package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

type launchPreflightRuntime struct {
	idleRuntime
	verify  func() error
	preview func(map[string]string) error
}

func (r launchPreflightRuntime) Preflight(context.Context) error { return r.verify() }
func (r launchPreflightRuntime) PreflightAdopted(_ context.Context, removals map[string]string) error {
	return r.preview(removals)
}
func (r launchPreflightRuntime) PreflightWithOwnedRemovals(_ context.Context, removals map[string]string) error {
	return r.preview(removals)
}

func TestApplyPreflightsWrittenOwnedAndDriftedAdoptedBeforeCommit(t *testing.T) {
	for _, drift := range []bool{false, true} {
		t.Run(map[bool]string{false: "valid mixed", true: "drifted adopted"}[drift], func(t *testing.T) {
			backend, home, request := fixture(t)
			backend.runCommand = fakeOwnedCommandFor(home)
			external := filepath.Join(home, "external.service")
			original := []byte("adopted launch fixture")
			if err := os.WriteFile(external, original, 0600); err != nil {
				t.Fatal(err)
			}
			adopted := request.Catalog.Profiles[0]
			adopted.NativeModel = &control.NativeModel{Runtime: "llama.cpp", Instance: "existing", Model: "selected", Endpoint: "http://127.0.0.1:8000", LaunchFile: external, LaunchSHA256: digest(original)}
			request.Catalog.Profiles = []control.WorkloadProfile{adopted}
			if err := backend.Apply(t.Context(), home, request); err != nil {
				t.Fatal(err)
			}
			request.ExpectedRevision = currentRevision(t, request)
			owned, raw := ownedFixtureProfile(t, home, "vision", 9100)
			request.Catalog.Version = 2
			request.Catalog.Profiles = append(request.Catalog.Profiles, owned)
			if drift {
				if err := os.WriteFile(external, []byte("drifted adopted launch"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			checks := 0
			backend.makeRuntime = func(Request) (gpuruntime.Manager, error) {
				return launchPreflightRuntime{verify: func() error {
					checks++
					data, err := os.ReadFile(filepath.Join(ownedUnitDirectory(home), owned.Unit))
					if err != nil || string(data) != string(raw) {
						t.Fatalf("preflight ran before owned writes: %v", err)
					}
					data, err = os.ReadFile(external)
					if err != nil || digest(data) != adopted.NativeModel.LaunchSHA256 {
						return gpuruntime.ErrLaunchChanged
					}
					return nil
				}}, nil
			}
			err := backend.Apply(t.Context(), home, request)
			if (err != nil) != drift {
				t.Fatalf("drift=%v: %v", drift, err)
			}
			snapshot, err := ReadCatalog(t.Context(), request.Profile.StatePath)
			if err != nil {
				t.Fatal(err)
			}
			if drift {
				if !reflect.DeepEqual(snapshot.Catalog.Profiles, []control.WorkloadProfile{adopted}) {
					t.Fatal("drifted adopted binding was committed")
				}
				if _, err := os.Stat(filepath.Join(ownedUnitDirectory(home), owned.Unit)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("uncommitted owned write not rolled back: %v", err)
				}
				if _, present, err := readOwnedUnitJournal(filepath.Join(home, ".config/gpu-workload-supervisor")); err != nil || present {
					t.Fatalf("rollback journal survived: %v %v", present, err)
				}
			} else if checks < 2 || len(snapshot.Catalog.Profiles) != 2 {
				t.Fatalf("mixed activation did not preflight before commit: checks=%d", checks)
			}
		})
	}
}

func TestAllOwnedActivationPreflightsNewUnitBeforeCatalogCommit(t *testing.T) {
	backend, home, request := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	owned, raw := ownedFixtureProfile(t, home, "vision", 9100)
	request.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{owned}}
	checks := 0
	backend.makeRuntime = func(Request) (gpuruntime.Manager, error) {
		return launchPreflightRuntime{verify: func() error {
			checks++
			data, err := os.ReadFile(filepath.Join(ownedUnitDirectory(home), owned.Unit))
			if err != nil || string(data) != string(raw) {
				t.Fatalf("owned binding not created before preflight: %v", err)
			}
			if checks == 1 {
				if _, err := os.Stat(request.Profile.StatePath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("state committed before initial preflight: %v", err)
				}
			}
			return nil
		}}, nil
	}
	if err := backend.Apply(t.Context(), home, request); err != nil {
		t.Fatal(err)
	}
	if checks < 2 {
		t.Fatal("owned activation skipped final precommit preflight")
	}
}

func TestPreviewAndApplyCarryOnlyAcceptedOwnedRemovalProofs(t *testing.T) {
	for _, change := range []string{"remove", "instance edit", "drifted removal"} {
		t.Run(change, func(t *testing.T) {
			backend, home, request := fixture(t)
			backend.runCommand = fakeOwnedCommandFor(home)
			draft := Draft{ID: "chat", Label: "Chat", App: "ollama", Model: "qwen:latest", Binding: &DraftBinding{Instance: "first", Owned: &DraftOwnedLaunch{Port: 11434}}}
			original, raw, err := OwnedProfile(draft, "/user.slice/user-1000.slice/user@1000.service", home)
			if err != nil {
				t.Fatal(err)
			}
			adopted := request.Catalog.Profiles[0]
			request.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{original, adopted}}
			if err := backend.Apply(t.Context(), home, request); err != nil {
				t.Fatal(err)
			}
			request.ExpectedRevision = currentRevision(t, request)
			request.Catalog.Profiles = []control.WorkloadProfile{adopted}
			if change == "instance edit" {
				draft.Binding.Instance = "second"
				replacement, _, err := OwnedProfile(draft, "/user.slice/user-1000.slice/user@1000.service", home)
				if err != nil {
					t.Fatal(err)
				}
				request.Catalog.Profiles = append(request.Catalog.Profiles, replacement)
			}
			expected := map[string]string{original.Unit: digest(raw)}
			checks := 0
			backend.makeRuntime = func(Request) (gpuruntime.Manager, error) {
				verify := func(removals map[string]string) error {
					checks++
					if !reflect.DeepEqual(removals, expected) {
						t.Fatalf("removal proof was not derived from accepted catalog: %v", removals)
					}
					data, err := os.ReadFile(filepath.Join(ownedUnitDirectory(home), original.Unit))
					if err != nil || digest(data) != expected[original.Unit] {
						return gpuruntime.ErrOrphanedOwnedUnit
					}
					return nil
				}
				return launchPreflightRuntime{verify: func() error { t.Fatal("plain preflight discarded pending removal proofs"); return nil }, preview: verify}, nil
			}
			oldPath := filepath.Join(ownedUnitDirectory(home), original.Unit)
			if change == "drifted removal" {
				if err := os.WriteFile(oldPath, []byte("foreign change"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			previewErr := backend.verifyBindings(t.Context(), home, request)
			if change == "drifted removal" {
				if !errors.Is(previewErr, ErrOwnedUnitModified) {
					t.Fatalf("drifted removal preview accepted: %v", previewErr)
				}
				if err := backend.Apply(t.Context(), home, request); !errors.Is(err, ErrOwnedUnitModified) {
					t.Fatalf("drifted removal applied: %v", err)
				}
				if checks != 0 {
					t.Fatal("drifted proof reached runtime as accepted removal")
				}
				return
			}
			if previewErr != nil {
				t.Fatal(previewErr)
			}
			if err := backend.Apply(t.Context(), home, request); err != nil {
				t.Fatal(err)
			}
			if checks < 3 {
				t.Fatalf("preview and final precommit checks missing: %d", checks)
			}
			if _, err := os.Stat(oldPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("proven old unit not removed after commit: %v", err)
			}
		})
	}
}
