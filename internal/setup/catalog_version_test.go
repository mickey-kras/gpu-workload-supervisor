package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/supervisor"
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
	// The write gate accepts limits. Preview and apply run the host
	// checks of the runtime.
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

// The setup checks of the runtime run before any unit write. A runtime
// without them refuses limits, and a failed check refuses the apply.
func TestSetupChecksLimitsBeforeUnitWrites(t *testing.T) {
	backend, home, _ := fixture(t)
	profile, _ := ownedFixtureProfile(t, home, "vision", 9100)
	request := ownedFixtureRequest(t, home, profile)
	request.Catalog.Version = control.CatalogVersionLimits
	request.Catalog.Profiles[0].NativeModel.Owned.Limits = control.ResourceLimits{Version: 1, TasksMax: 4096}
	refingerprint(t, &request.Catalog.Profiles[0])
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(bytes.NewReader(raw)); err != nil {
		t.Fatalf("decode with limits = %v", err)
	}
	if _, err := backend.Plan(home, request); !errors.Is(err, gpuruntime.ErrLimitUnchecked) {
		t.Fatalf("preview with limits and no checks = %v", err)
	}
	request.ConfirmQuiesced = true
	backend.makeRuntime = func(Request) (supervisor.Runtime, error) {
		return limitRuntime{checkErr: gpuruntime.ErrHostCap}, nil
	}
	if err := backend.Apply(t.Context(), home, request); !errors.Is(err, gpuruntime.ErrHostCap) || !strings.Contains(err.Error(), "vision (") {
		t.Fatalf("apply with a failed host check = %v", err)
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

// acceptLimitedCatalog stands in for a later release: it commits limits on
// the applied owned profile and writes the unit with the limits block.
func acceptLimitedCatalog(t *testing.T, home string, r Request) control.WorkloadProfile {
	t.Helper()
	ctx := context.Background()
	s := openStoreAt(t, r.Profile.StatePath)
	defer s.Close()
	snap, err := s.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	limited := snap.Catalog.Clone()
	limited.Version = control.CatalogVersionLimits
	limited.Profiles[0].NativeModel.Owned.Limits = control.ResourceLimits{Version: 1, TasksMax: 4096}
	refingerprint(t, &limited.Profiles[0])
	raw, err := gpuruntime.RenderOwnedUnit(limited.Profiles[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ownedUnitDirectory(home), limited.Profiles[0].Unit), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplaceCatalog(ctx, snap.Revision, limited); err != nil {
		t.Fatal(err)
	}
	return limited.Profiles[0]
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

func TestSetupRemovesLimitsThatALaterReleaseWrote(t *testing.T) {
	for name, keep := range map[string]bool{"remove the workload": false, "keep it without limits": true} {
		t.Run(name, func(t *testing.T) {
			backend, home, r := fixture(t)
			backend.runCommand = fakeOwnedCommandFor(home)
			profile, plainUnit := ownedFixtureProfile(t, home, "vision", 9100)
			r.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}
			ctx := context.Background()
			if err := backend.Apply(ctx, home, r); err != nil {
				t.Fatal(err)
			}
			limited := acceptLimitedCatalog(t, home, r)
			r.ExpectedRevision = acceptedRevision(t, r).Revision
			// Keeping the limits needs the host checks, which the fixture
			// runtime cannot run, so the apply is refused before writes.
			r.Catalog = control.Catalog{Version: 5, Profiles: []control.WorkloadProfile{limited}}
			if err := backend.Apply(ctx, home, r); !errors.Is(err, gpuruntime.ErrLimitUnchecked) {
				t.Fatalf("apply that keeps limits = %v", err)
			}
			want := 1
			r.Catalog = control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "text", Label: "Text", Adapter: "systemd", Unit: "text.service", Cgroup: "/user.slice/text", HealthURL: "http://127.0.0.1:8000/health"}}}
			if keep {
				want = 2
				r.Catalog = control.Catalog{Version: 5, Profiles: []control.WorkloadProfile{profile}}
			}
			if _, err := backend.Plan(home, r); err != nil {
				t.Fatalf("preview that removes limits: %v", err)
			}
			if err := backend.Apply(ctx, home, r); err != nil {
				t.Fatalf("apply that removes limits: %v", err)
			}
			accepted := acceptedRevision(t, r).Catalog
			if accepted.Version != want || accepted.HasLimits() {
				t.Fatalf("catalog after removal: version %d, limits %v", accepted.Version, accepted.HasLimits())
			}
			unit, err := os.ReadFile(filepath.Join(ownedUnitDirectory(home), profile.Unit))
			if keep && (err != nil || !bytes.Equal(unit, plainUnit)) {
				t.Fatalf("unit without limits not restored: %v", err)
			}
			if !keep && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("removed workload unit remains: %v", err)
			}
		})
	}
}

func TestTrainerInspectionIgnoresAnUnrelatedLimitedProfile(t *testing.T) {
	b, request := trainerInspectionRequest(t)
	limited, _ := ownedFixtureProfile(t, t.TempDir(), "vision", 9100)
	limited.NativeModel.Owned.Limits = control.ResourceLimits{Version: 1, TasksMax: 4096}
	refingerprint(t, &limited)
	request.Catalog.Version = control.CatalogVersionLimits
	request.Catalog.Profiles = append(request.Catalog.Profiles, limited)
	inspected := false
	b.makeRuntime = func(Request) (supervisor.Runtime, error) {
		return trainerInspector{inspect: func(context.Context, control.Workload) error { inspected = true; return nil }}, nil
	}
	if _, err := b.InspectTrainer(context.Background(), request, "trainer"); err != nil || !inspected {
		t.Fatalf("trainer inspection with a limited profile = %v, inspected %v", err, inspected)
	}
	if err := Validate(request); err != nil {
		t.Fatalf("write gate refused limits: %v", err)
	}
}

func TestSetupRejectsUnsupportedVersionBeforeNormalizing(t *testing.T) {
	backend, home, _ := fixture(t)
	backend.makeRuntime = func(Request) (supervisor.Runtime, error) {
		t.Fatal("setup reached the runtime with an unsupported version")
		return nil, nil
	}
	profile, _ := ownedFixtureProfile(t, home, "vision", 9100)
	for _, version := range []int{0, 3, 6, -1} {
		request := ownedFixtureRequest(t, home, profile)
		request.Catalog.Version = version
		raw, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Decode(bytes.NewReader(raw)); !errors.Is(err, control.ErrUnsupportedCatalogVersion) {
			t.Fatalf("decode with version %d = %v", version, err)
		}
		if _, err := backend.Plan(home, request); !errors.Is(err, control.ErrUnsupportedCatalogVersion) {
			t.Fatalf("preview with version %d = %v", version, err)
		}
		if err := backend.Apply(t.Context(), home, request); !errors.Is(err, control.ErrUnsupportedCatalogVersion) {
			t.Fatalf("apply with version %d = %v", version, err)
		}
		if _, err := os.Stat(request.Catalog.Profiles[0].NativeModel.LaunchFile); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unit written for version %d: %v", version, err)
		}
	}
	// An omitted version decodes as zero and fails the same way.
	request := ownedFixtureRequest(t, home, profile)
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	omitted := strings.Replace(string(raw), `"catalog":{"version":2,`, `"catalog":{`, 1)
	if omitted == string(raw) {
		t.Fatal("fixture has no catalog version key")
	}
	if _, err := Decode(strings.NewReader(omitted)); !errors.Is(err, control.ErrUnsupportedCatalogVersion) {
		t.Fatalf("decode without version = %v", err)
	}
}

// After a limit rejection, reconcile leaves the state latched in error, and
// setup refuses it. The test writes the state that recovery settles, and
// setup then applies the request that removes the limits.
func TestSetupRemovesLimitsAfterRecovery(t *testing.T) {
	backend, home, r := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	profile, _ := ownedFixtureProfile(t, home, "vision", 9100)
	r.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}
	ctx := context.Background()
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	acceptLimitedCatalog(t, home, r)
	r.ExpectedRevision = acceptedRevision(t, r).Revision
	setState := func(health control.Health, phase control.Phase) {
		t.Helper()
		s := openStoreAt(t, r.Profile.StatePath)
		defer s.Close()
		state, err := s.State(ctx)
		if err != nil {
			t.Fatal(err)
		}
		state.Health, state.Phase, state.Admission = health, phase, control.AdmissionClosed
		state.ActiveWorkload, state.DesiredWorkload = control.WorkloadIdle, control.WorkloadIdle
		if _, err := s.Recover(ctx, state.Version, state, "test"); err != nil {
			t.Fatal(err)
		}
	}
	r.Catalog = control.Catalog{Version: 5, Profiles: []control.WorkloadProfile{profile}}
	setState(control.HealthError, control.PhaseReconciling)
	latchedErr := backend.Apply(ctx, home, r)
	if latchedErr == nil || !strings.Contains(latchedErr.Error(), "requires explicitly quiesced healthy Idle") || !acceptedRevision(t, r).Catalog.HasLimits() {
		t.Fatalf("apply while latched = %v", latchedErr)
	}
	// The state that recovery settles: stable, healthy, closed Idle.
	setState(control.HealthHealthy, control.PhaseStable)
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatalf("apply that removes limits after recovery = %v (latched: %v)", err, latchedErr)
	}
	if accepted := acceptedRevision(t, r).Catalog; accepted.HasLimits() {
		t.Fatal("limits remain after the removal")
	}
}

// Setup checks a healthy, stable Idle with closed admission, not the owner:
// a user-owned Idle can remove limits, so the next steps need not return
// control to the supervisor first.
func TestSetupRemovesLimitsFromUserOwnedIdle(t *testing.T) {
	backend, home, r := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	profile, _ := ownedFixtureProfile(t, home, "vision", 9100)
	r.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}
	ctx := context.Background()
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	acceptLimitedCatalog(t, home, r)
	r.ExpectedRevision = acceptedRevision(t, r).Revision
	s := openStoreAt(t, r.Profile.StatePath)
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state.Owner, state.Health, state.Phase, state.Admission = control.OwnerUser, control.HealthHealthy, control.PhaseStable, control.AdmissionClosed
	state.ActiveWorkload, state.DesiredWorkload = control.WorkloadIdle, control.WorkloadIdle
	if _, err := s.Recover(ctx, state.Version, state, "test"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	r.Catalog = control.Catalog{Version: 5, Profiles: []control.WorkloadProfile{profile}}
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatalf("apply that removes limits from a user-owned Idle = %v", err)
	}
	if acceptedRevision(t, r).Catalog.HasLimits() {
		t.Fatal("limits remain after the removal")
	}
}
