package setup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/supervisor"
)

func TestLimitsRaiseAndRestoreCatalogVersion(t *testing.T) {
	_, home, _ := fixture(t)
	profile, _ := ownedFixtureProfile(t, home, "vision", 9100)
	request := ownedFixtureRequest(t, home, profile)
	prepared, err := prepareOwnedBackstops(request)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Catalog.Version != control.CatalogVersionOwned {
		t.Fatalf("owned catalog version = %d, want %d", prepared.Catalog.Version, control.CatalogVersionOwned)
	}

	limited := prepared
	limited.Catalog = prepared.Catalog.Clone()
	limited.Catalog.Profiles[0].NativeModel.Owned.Limits = control.ResourceLimits{Version: 1, MemoryMaxMiB: 8192, TasksMax: 4096}
	refingerprint(t, &limited.Catalog.Profiles[0])
	limited, err = prepareOwnedBackstops(limited)
	if err != nil {
		t.Fatal(err)
	}
	if limited.Catalog.Version != control.CatalogVersionLimits || limited.Catalog.Profiles[0].NativeModel.LaunchSHA256 == profile.NativeModel.LaunchSHA256 {
		t.Fatalf("limits did not raise the version or the fingerprint: %+v", limited.Catalog.Profiles[0].NativeModel)
	}
	if err := Validate(limited); err != nil {
		t.Fatalf("v5 request with limits = %v", err)
	}

	restored := limited
	restored.Catalog = limited.Catalog.Clone()
	restored.Catalog.Profiles[0].NativeModel.Owned.Limits = control.ResourceLimits{}
	refingerprint(t, &restored.Catalog.Profiles[0])
	restored, err = prepareOwnedBackstops(restored)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Catalog.Version != control.CatalogVersionOwned || restored.Catalog.Profiles[0].NativeModel.LaunchSHA256 != profile.NativeModel.LaunchSHA256 {
		t.Fatalf("clearing limits did not restore v2 owned unit: %+v", restored.Catalog)
	}
}

func refingerprint(t *testing.T, p *control.WorkloadProfile) {
	t.Helper()
	raw, err := gpuruntime.RenderOwnedUnit(*p)
	if err != nil {
		t.Fatal(err)
	}
	p.NativeModel.LaunchSHA256 = digest(raw)
}

func acceptedRevision(t *testing.T, r Request) control.CatalogSnapshot {
	t.Helper()
	s := openStoreAt(t, r.Profile.StatePath)
	defer s.Close()
	snap, err := s.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestSetupAppliesAndRemovesSystemdResourceLimits(t *testing.T) {
	backend, home, r := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	backend.makeRuntime = func(Request) (supervisor.Runtime, error) { return idleRuntime{}, nil }
	profile, plainUnit := ownedFixtureProfile(t, home, "vision", 9100)
	limited := profile
	limited.NativeModel.Owned.Limits = control.ResourceLimits{Version: 1, TasksMax: 4096}
	refingerprint(t, &limited)
	r.Catalog = control.Catalog{Version: control.CatalogVersionLimits, Profiles: []control.WorkloadProfile{limited}}
	ctx := context.Background()
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatalf("Apply with limits: %v", err)
	}
	unitBytes, err := os.ReadFile(filepath.Join(ownedUnitDirectory(home), profile.Unit))
	if err != nil || !strings.Contains(string(unitBytes), "TasksMax=4096") {
		t.Fatalf("expected rendered TasksMax in unit, got %q, %v", string(unitBytes), err)
	}
	if _, err := Decode(strings.NewReader(`{"version":1,"profile":{"version":1,"statePath":"/s","systemctlPath":"/bin/systemctl","nvidiaSMIPath":"/bin/nvidia-smi"},"catalog":{"version":5,"profiles":[{"id":"vision","label":"Vision","adapter":"systemd","unit":"gws-owned-vision.service","cgroup":"/u/app.slice/gws-owned-vision.service","healthURL":"http://127.0.0.1:9100/health","nativeModel":{"runtime":"vllm","instance":"local","endpoint":"http://127.0.0.1:9100","model":"m","gpuUUID":"GPU-11111111-2222-3333-4444-555555555555","launchFile":"/h/.config/systemd/user/gws-owned-vision.service","launchSHA256":"` + strings.Repeat("a", 64) + `","chatTemplate":"/m/t.jinja","chatTemplateSHA256":"` + strings.Repeat("b", 64) + `","environmentFile":"/h/.config/gpu-workload-supervisor/keys/vision.env","owned":{"modelPath":"/m","port":9100,"limits":{}}}}]}}`)); !errors.Is(err, control.ErrEmptyLimits) {
		t.Fatalf("expected ErrEmptyLimits, got %v", err)
	}
	r.ExpectedRevision = acceptedRevision(t, r).Revision
	r.Catalog = control.Catalog{Version: control.CatalogVersionOwned, Profiles: []control.WorkloadProfile{profile}}
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatalf("Apply removing limits: %v", err)
	}
	restoredUnit, err := os.ReadFile(filepath.Join(ownedUnitDirectory(home), profile.Unit))
	if err != nil || !bytes.Equal(restoredUnit, plainUnit) {
		t.Fatalf("unit without limits not restored: %v", err)
	}
}
