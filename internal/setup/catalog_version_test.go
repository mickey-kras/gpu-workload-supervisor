package setup

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

func TestPreparedCatalogUsesLowestVersion(t *testing.T) {
	home := t.TempDir()
	profile, legacyUnit := ownedFixtureProfile(t, home, "vision", 9100)
	request := ownedFixtureRequest(t, home, profile)
	request.Catalog.Version = 4
	prepared, err := prepareOwnedBackstops(request)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Catalog.Version != 2 || request.Catalog.Version != 4 {
		t.Fatalf("prepared version = %d, request version = %d", prepared.Catalog.Version, request.Catalog.Version)
	}
	original, err := json.Marshal(prepared.Catalog)
	if err != nil {
		t.Fatal(err)
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
		t.Fatalf("v5 request rejected: %v", err)
	}

	restored := limited
	restored.Catalog = limited.Catalog.Clone()
	restored.Catalog.Profiles[0].NativeModel.Owned.Limits = control.ResourceLimits{}
	refingerprint(t, &restored.Catalog.Profiles[0])
	restored, err = prepareOwnedBackstops(restored)
	if err != nil {
		t.Fatal(err)
	}
	again, err := json.Marshal(restored.Catalog)
	if err != nil || !bytes.Equal(original, again) {
		t.Fatalf("rollback bytes differ:\n%s\n%s", original, again)
	}
	unit, err := gpuruntime.RenderOwnedUnit(restored.Catalog.Profiles[0])
	if err != nil || !bytes.Equal(unit, legacyUnit) {
		t.Fatalf("rollback unit bytes differ: %v", err)
	}
}

// refingerprint records the spec edit the way setup preview does.
func refingerprint(t *testing.T, p *control.WorkloadProfile) {
	t.Helper()
	raw, err := gpuruntime.RenderOwnedUnit(*p)
	if err != nil {
		t.Fatal(err)
	}
	p.NativeModel.LaunchSHA256 = digest(raw)
}
