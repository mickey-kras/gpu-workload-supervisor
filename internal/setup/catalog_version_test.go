package setup

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
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
	// This release cannot verify limits, so setup refuses them before any
	// unit write and names the workload.
	if err := Validate(limited); !errors.Is(err, ErrLimitsNotInSetup) || !strings.Contains(err.Error(), "vision") {
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
	again, err := json.Marshal(restored.Catalog)
	if err != nil || !bytes.Equal(original, again) {
		t.Fatalf("rollback bytes differ:\n%s\n%s", original, again)
	}
	unit, err := gpuruntime.RenderOwnedUnit(restored.Catalog.Profiles[0])
	if err != nil || !bytes.Equal(unit, legacyUnit) {
		t.Fatalf("rollback unit bytes differ: %v", err)
	}
	// The request that removes the limits passes, so setup can recover a
	// catalog with limits that a later release wrote.
	if err := Validate(restored); err != nil {
		t.Fatalf("request without limits rejected: %v", err)
	}
}

func TestSetupRejectsLimitsBeforeUnitWrites(t *testing.T) {
	backend, home, _ := fixture(t)
	backend.makeRuntime = func(Request) (gpuruntime.Manager, error) {
		t.Fatal("setup reached the runtime with limits")
		return nil, nil
	}
	profile, _ := ownedFixtureProfile(t, home, "vision", 9100)
	request := ownedFixtureRequest(t, home, profile)
	request.Catalog.Version = control.CatalogVersionLimits
	request.Catalog.Profiles[0].NativeModel.Owned.Limits = control.ResourceLimits{Version: 1, TasksMax: 4096}
	refingerprint(t, &request.Catalog.Profiles[0])
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(bytes.NewReader(raw)); !errors.Is(err, ErrLimitsNotInSetup) {
		t.Fatalf("decode with limits = %v", err)
	}
	if _, err := backend.Plan(home, request); !errors.Is(err, ErrLimitsNotInSetup) {
		t.Fatalf("preview with limits = %v", err)
	}
	if err := backend.Apply(t.Context(), home, request); !errors.Is(err, ErrLimitsNotInSetup) {
		t.Fatalf("apply with limits = %v", err)
	}
	if _, err := os.Stat(request.Catalog.Profiles[0].NativeModel.LaunchFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unit written for a rejected request: %v", err)
	}
	// Empty forms fail in the setup decoder too, not only in the catalog
	// decoder, so a key that omitzero would drop is never accepted.
	for _, empty := range []string{`{}`, `null`, `{"version":0}`} {
		smuggled := strings.Replace(string(raw), `"limits":{"version":1,"tasksMax":4096}`, `"limits":`+empty, 1)
		if smuggled == string(raw) {
			t.Fatal("fixture has no limits key")
		}
		if _, err := Decode(strings.NewReader(smuggled)); !errors.Is(err, control.ErrEmptyLimits) {
			t.Fatalf("decode with limits %s = %v", empty, err)
		}
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
