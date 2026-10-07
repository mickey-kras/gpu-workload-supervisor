package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

func ownedCLIProfile() control.WorkloadProfile {
	return control.WorkloadProfile{
		ID:        "vision",
		Label:     "Vision",
		Adapter:   "systemd",
		Unit:      "gws-owned-vision.service",
		Cgroup:    "/user.slice/user-1000.slice/user@1000.service/app.slice/gws-owned-vision.service",
		HealthURL: "http://127.0.0.1:9100/health",
		NativeModel: &control.NativeModel{
			Runtime:      "llama.cpp",
			Instance:     "owned",
			Model:        "vision-q8",
			Endpoint:     "http://127.0.0.1:9100",
			LaunchFile:   "/home/u/.config/systemd/user/gws-owned-vision.service",
			LaunchSHA256: strings.Repeat("a", 64),
			Owned:        &control.OwnedLaunch{ModelPath: "/models/vision-q8.gguf", Port: 9100, Alias: "vision-q8"},
		},
	}
}

func TestOwnedProfilesVerbatimGate(t *testing.T) {
	owned := ownedCLIProfile()
	accepted := control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{owned}}

	t.Run("carried verbatim accepted", func(t *testing.T) {
		next := accepted.Clone()
		if err := ownedProfilesVerbatim(accepted, next); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("no owned profiles either side", func(t *testing.T) {
		plain := control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "text"}}}
		if err := ownedProfilesVerbatim(plain, plain.Clone()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("add rejected", func(t *testing.T) {
		if err := ownedProfilesVerbatim(control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "text"}}}, accepted.Clone()); !errors.Is(err, control.ErrOwnedCatalogManagedBySetup) {
			t.Fatalf("add = %v", err)
		}
	})
	t.Run("remove rejected", func(t *testing.T) {
		next := control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "text"}}}
		if err := ownedProfilesVerbatim(accepted, next); !errors.Is(err, control.ErrOwnedCatalogManagedBySetup) {
			t.Fatalf("remove = %v", err)
		}
	})
	t.Run("modify rejected", func(t *testing.T) {
		next := accepted.Clone()
		next.Profiles[0].NativeModel.Owned.CtxSize = 4096
		if err := ownedProfilesVerbatim(accepted, next); !errors.Is(err, control.ErrOwnedCatalogManagedBySetup) {
			t.Fatalf("modify = %v", err)
		}
	})
	t.Run("owned to adopted conversion rejected", func(t *testing.T) {
		next := accepted.Clone()
		next.Profiles[0].NativeModel.Owned = nil
		if err := ownedProfilesVerbatim(accepted, next); !errors.Is(err, control.ErrOwnedCatalogManagedBySetup) {
			t.Fatalf("conversion = %v", err)
		}
	})
}

func TestConfigureCarriesOwnedProfilesVerbatim(t *testing.T) {
	oldArgs, oldStdout := os.Args, os.Stdout
	t.Cleanup(func() { os.Args, os.Stdout = oldArgs, oldStdout })
	out, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	os.Stdout = out
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.db")
	file := filepath.Join(dir, "catalog.json")
	r := &cliCatalogRuntime{}
	factory := func(cfg gpuruntime.SystemdConfig) (gpuruntime.Manager, error) { return r, nil }
	invoke := func(args ...string) error {
		os.Args = append([]string{"gpu-mode", "-state", statePath}, args...)
		return runWithRuntimeFactory(factory)
	}
	write := func(c control.Catalog) {
		t.Helper()
		raw, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	owned := ownedCLIProfile()
	v2 := control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{owned}}
	write(v2)
	// The v2 catalog with an owned profile enters through the setup path; the
	// CLI accepts it verbatim against an empty accepted catalog only via the
	// store, so seed it directly.
	s, err := store.Open(context.Background(), statePath)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := s.ReplaceCatalog(context.Background(), "", v2)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	t.Run("verbatim reconfigure accepted", func(t *testing.T) {
		write(v2)
		if err := invoke("-catalog", file, "-configuration-revision", snap.Revision, "configure"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("mutation rejected", func(t *testing.T) {
		mutated := v2.Clone()
		mutated.Profiles[0].NativeModel.Owned.GPULayers = 12
		write(mutated)
		if err := invoke("-catalog", file, "-configuration-revision", snap.Revision, "configure"); !errors.Is(err, control.ErrOwnedCatalogManagedBySetup) {
			t.Fatalf("mutation = %v", err)
		}
	})
	t.Run("removal rejected", func(t *testing.T) {
		write(control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "text", Label: "Text", Adapter: "systemd", Unit: "text.service", Cgroup: "/user/text", HealthURL: "http://localhost:9000"}}})
		if err := invoke("-catalog", file, "-configuration-revision", snap.Revision, "configure"); !errors.Is(err, control.ErrOwnedCatalogManagedBySetup) {
			t.Fatalf("removal = %v", err)
		}
	})
}
