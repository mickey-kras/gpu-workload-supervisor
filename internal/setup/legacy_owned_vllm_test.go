package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

// strictVLLMRuntime models the runtime qualification: a vLLM binding without
// the hardened settings never passes preflight.
func strictVLLMRuntime(request Request) (gpuruntime.Manager, error) {
	for _, p := range request.Catalog.Profiles {
		if p.NativeModel != nil && p.NativeModel.Runtime == "vllm" && !p.NativeModel.Hardened() {
			return idleRuntime{err: errors.New("binding from an earlier release")}, nil
		}
	}
	return idleRuntime{}, nil
}

// legacyOwnedVLLM accepts an owned vLLM binding as an earlier release wrote
// it: no hardened settings, earlier unit bytes and fingerprint.
func legacyOwnedVLLM(t *testing.T) (Backend, string, Request, control.WorkloadProfile) {
	t.Helper()
	b, home, request := fixture(t)
	b.runCommand = fakeOwnedCommandFor(home)
	profile, _ := ownedFixtureProfile(t, home, "vision", 9100)
	profile.NativeModel.ChatTemplate, profile.NativeModel.ChatTemplateSHA256, profile.NativeModel.EnvironmentFile = "", "", ""
	raw, err := gpuruntime.RenderOwnedUnit(profile)
	if err != nil {
		t.Fatal(err)
	}
	profile.NativeModel.LaunchSHA256 = digest(raw)
	request.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}
	if err := b.Apply(t.Context(), home, request); err != nil {
		t.Fatal(err)
	}
	snapshot, err := ReadCatalog(t.Context(), request.Profile.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	request.ExpectedRevision = snapshot.Revision
	b.makeRuntime = strictVLLMRuntime
	return b, home, request, profile
}

func TestLegacyOwnedVLLMBindingCanBeSetUpAgain(t *testing.T) {
	b, home, request, legacy := legacyOwnedVLLM(t)
	path := filepath.Join(ownedUnitDirectory(home), legacy.Unit)
	request.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{legacy}}
	if err := b.Apply(t.Context(), home, request); err == nil {
		t.Fatal("legacy binding kept for execution")
	}
	replacement, raw := ownedFixtureProfile(t, home, "vision", 9100)
	request.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{replacement}}
	if err := b.Apply(t.Context(), home, request); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(path)
	if err != nil || digest(written) != digest(raw) {
		t.Fatal("hardened unit not written", err)
	}
	if _, err := os.Stat(replacement.NativeModel.EnvironmentFile); err != nil {
		t.Fatal("key file not created on apply", err)
	}
}

func TestLegacyOwnedVLLMBindingCanBeRemoved(t *testing.T) {
	b, home, request, legacy := legacyOwnedVLLM(t)
	path := filepath.Join(ownedUnitDirectory(home), legacy.Unit)
	request.Catalog = control.Catalog{Version: 2, Disabled: true}
	if err := b.Apply(t.Context(), home, request); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("legacy unit survived removal", err)
	}
}

func TestLegacyAdoptedVLLMBindingDoesNotBlockRemoval(t *testing.T) {
	c := control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "chat", NativeModel: &control.NativeModel{Runtime: "vllm"}}}}
	if !hasRemovedRuntime(c) || removalObservationCatalog(c).Profiles[0].NativeModel != nil {
		t.Fatal("legacy adopted binding observed for execution")
	}
	runtime, _ := strictVLLMRuntime(Request{Catalog: removalObservationCatalog(c)})
	if err := runtime.Preflight(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestOwnedKeyFileIsCreatedOnApplyAndRemovedWithWorkload(t *testing.T) {
	b, home, request := fixture(t)
	b.runCommand = fakeOwnedCommandFor(home)
	profile, _ := ownedFixtureProfile(t, home, "vision", 9100)
	request.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}
	key := profile.NativeModel.EnvironmentFile
	plan := unitPlan{Writes: map[string][]byte{}, keys: map[string]string{}, createdKeys: map[string]bool{}}
	if err := b.planOwnedUnitWrites(request, home, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(key); !errors.Is(err, os.ErrNotExist) || plan.keys[profile.Unit] != key {
		t.Fatal("planning created the key file", err)
	}
	if err := b.Apply(t.Context(), home, request); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(key); err != nil {
		t.Fatal("apply did not create the key file", err)
	}
	snapshot, err := ReadCatalog(t.Context(), request.Profile.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	request.ExpectedRevision = snapshot.Revision
	request.Catalog = control.Catalog{Version: 2, Disabled: true}
	if err := b.Apply(t.Context(), home, request); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(key); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("key file survived workload removal", err)
	}
}

func TestRollbackRemovesOnlyCreatedKeyFiles(t *testing.T) {
	dir := t.TempDir()
	created, kept := filepath.Join(dir, "created.env"), filepath.Join(dir, "kept.env")
	plan := unitPlan{keys: map[string]string{"a.service": created, "b.service": kept}, createdKeys: map[string]bool{}}
	if err := os.WriteFile(kept, []byte("VLLM_API_KEY=keptkeptkeptkeptkeptkeptkeptkeptkept\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureOwnedKey(plan, "a.service"); err != nil {
		t.Fatal(err)
	}
	if err := ensureOwnedKey(plan, "b.service"); err != nil {
		t.Fatal(err)
	}
	if err := ensureOwnedKey(plan, "c.service"); err != nil {
		t.Fatal(err)
	}
	if !plan.createdKeys["a.service"] || plan.createdKeys["b.service"] {
		t.Fatalf("created keys = %v", plan.createdKeys)
	}
	removeCreatedKeys(plan)
	if _, err := os.Lstat(created); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("created key file survived rollback")
	}
	if _, err := os.Stat(kept); err != nil {
		t.Fatal("existing key file removed", err)
	}
	if err := ensureOwnedKey(unitPlan{keys: map[string]string{"x.service": filepath.Join(kept, "nested.env")}}, "x.service"); err == nil {
		t.Fatal("key creation failure ignored")
	}
	if err := removeOwnedKey(t.TempDir(), control.OwnedUnitName("absent")); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	busy := OwnedEnvironmentFile(home, "busy")
	if err := os.MkdirAll(filepath.Join(busy, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := removeOwnedKey(home, control.OwnedUnitName("busy")); err == nil {
		t.Fatal("key removal failure ignored")
	}
}

func TestOwnedKeyFileMustLiveUnderTheSetupHome(t *testing.T) {
	_, home, _ := fixture(t)
	profile, _ := ownedFixtureProfile(t, home, "vision", 9100)
	profile.NativeModel.EnvironmentFile = "/elsewhere/vision.env"
	plan := unitPlan{keys: map[string]string{}}
	if err := pinOwnedFiles(profile, home, plan); !errors.Is(err, ErrOwnedEnvironmentFileOutsideHome) {
		t.Fatalf("foreign key file accepted: %v", err)
	}
}
