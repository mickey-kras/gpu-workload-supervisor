package setup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

func TestStalePreviewDoesNotEnterMaintenance(t *testing.T) {
	backend, home, r := fixture(t)
	ctx := context.Background()
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	snapshot, err := ReadCatalog(ctx, r.Profile.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	r.Catalog.Profiles[0].Label = "Changed"
	if err := backend.Apply(ctx, home, r); err == nil {
		t.Fatal("stale revision accepted")
	}
	if err := deployment.Check(r.Profile.StatePath, deployment.Release); err != nil {
		t.Fatal("stale preview wedged deployment", err)
	}
	accepted, _ := ReadCatalog(ctx, r.Profile.StatePath)
	if accepted.Revision != snapshot.Revision {
		t.Fatal("catalog changed")
	}
	r.ExpectedRevision = snapshot.Revision
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal("fresh preview cannot apply", err)
	}
}
func TestDiscoveryAndOldRuntimeUseCommittedDatabaseCatalog(t *testing.T) {
	backend, home, r := fixture(t)
	ctx := context.Background()
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, r.Profile.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	prior, _ := s.Catalog(ctx)
	replacement := r.Catalog
	replacement.Profiles = append([]control.Profile(nil), r.Catalog.Profiles...)
	replacement.Profiles[0].Unit = "changed.service"
	replacement.Profiles[0].Label = "Accepted through CLI"
	accepted, err := s.ReplaceCatalog(ctx, prior.Revision, replacement)
	s.Close()
	if err != nil {
		t.Fatal(err)
	}
	found, err := backend.Discover(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	if found.Request.ExpectedRevision != accepted.Revision || found.Request.Catalog.Profiles[0].Unit != "changed.service" {
		t.Fatalf("stale mirror won: %+v", found)
	}
	var checked []string
	backend.makeRuntime = func(request Request) (gpuruntime.Manager, error) {
		checked = append(checked, request.Catalog.Profiles[0].Unit)
		return idleRuntime{}, nil
	}
	r.ExpectedRevision = accepted.Revision
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	if len(checked) < 2 || checked[1] != "changed.service" {
		t.Fatalf("old mapping not checked: %v", checked)
	}
}
func TestFreshActivationResumesAfterDatabaseCreation(t *testing.T) {
	backend, home, r := fixture(t)
	ctx := context.Background()
	root := filepath.Join(home, ".config/gpu-workload-supervisor")
	if err := mkdirTrusted(root); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, r.Profile.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := writeJSON(filepath.Join(root, "activation.json"), activation{Request: r, Fresh: true}); err != nil {
		t.Fatal(err)
	}
	if err := deployment.Write(r.Profile.StatePath, deployment.Marker{Version: 1, Release: deployment.Release, Maintenance: true}); err != nil {
		t.Fatal(err)
	}
	found, err := backend.Discover(ctx, home)
	if err != nil || !found.Pending {
		t.Fatalf("pending setup unavailable: %+v %v", found, err)
	}
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	s, err = store.Open(ctx, r.Profile.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	state, _ := s.State(ctx)
	if state.ActiveWorkload != control.WorkloadIdle || state.Phase != control.PhaseStable || state.Admission != control.AdmissionClosed {
		t.Fatalf("fresh normalization skipped: %+v", state)
	}
}
func TestSameCatalogProfileChangeResumesBeforeOwnedFiles(t *testing.T) {
	backend, home, r := fixture(t)
	ctx := context.Background()
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, ".config/gpu-workload-supervisor")
	snapshot, _ := ReadCatalog(ctx, r.Profile.StatePath)
	r.ExpectedRevision = snapshot.Revision
	r.Profile.GPUIndex = 1
	beforeProfile, _ := privateRead(filepath.Join(root, "operator.json"))
	beforeCatalog, _ := privateRead(filepath.Join(root, "catalog.json"))
	profile := r.Profile
	profile.ActivatedRelease = deployment.Release
	afterProfile, _ := json.Marshal(profile)
	afterCatalog, _ := json.Marshal(r.Catalog)
	pending := journal{Version: 1, Files: map[string]entry{"operator.json": {Before: beforeProfile, Existed: true, After: afterProfile}, "catalog.json": {Before: beforeCatalog, Existed: true, After: afterCatalog}}}
	if err := writeJSON(filepath.Join(root, journalName), pending); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(root, "activation.json"), activation{Request: r, Fresh: false}); err != nil {
		t.Fatal(err)
	}
	if err := deployment.Write(r.Profile.StatePath, deployment.Marker{Version: 1, Release: deployment.Release, Maintenance: true}); err != nil {
		t.Fatal(err)
	}
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	data, _ := privateRead(filepath.Join(root, "operator.json"))
	var got Profile
	json.Unmarshal(data, &got)
	if got.GPUIndex != 1 {
		t.Fatal("profile change not completed")
	}
}
func TestPendingDiscoveryPrecedesIncompleteProfileAndCatalog(t *testing.T) {
	backend, home, r := fixture(t)
	ctx := context.Background()
	root := filepath.Join(home, ".config/gpu-workload-supervisor")
	mkdirTrusted(root)
	s, err := store.Open(ctx, r.Profile.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	writeJSON(filepath.Join(root, "operator.json"), r.Profile)
	writeJSON(filepath.Join(root, "activation.json"), activation{Request: r, Fresh: true})
	deployment.Write(r.Profile.StatePath, deployment.Marker{Version: 1, Release: deployment.Release, Maintenance: true})
	discovery, err := backend.Discover(ctx, home)
	if err != nil || !discovery.Pending {
		t.Fatalf("%+v %v", discovery, err)
	}
}
func TestEachActivationRetainsItsOwnSnapshot(t *testing.T) {
	backend, home, r := fixture(t)
	ctx := context.Background()
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := ReadCatalog(ctx, r.Profile.StatePath)
	r.ExpectedRevision = snapshot.Revision
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	backups, err := os.ReadDir(filepath.Join(home, ".config/gpu-workload-supervisor/backups"))
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 2 {
		t.Fatalf("activation snapshots reused: %d", len(backups))
	}
	for _, backup := range backups {
		if _, err := os.Stat(filepath.Join(home, ".config/gpu-workload-supervisor/backups", backup.Name(), "state.db"+deployment.Suffix)); err != nil {
			t.Fatal("activation marker not backed up", err)
		}
	}
}

func TestPostcommitFileFailureNeverRollsBack(t *testing.T) {
	root := t.TempDir()
	tx := Transaction{Root: root, Changes: map[string][]byte{"integration": []byte("new")}}
	pending := journal{Version: 1, Files: map[string]entry{"integration": {Existed: true, Before: []byte("old"), After: []byte("new")}}}
	if err := writeJSON(filepath.Join(root, journalName), pending); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "integration"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	tx.write = func(string, []byte) error { return os.ErrPermission }
	hooks := Hooks{Quiescent: func() error { return nil }, Committed: func() (bool, error) { return true, nil }, Commit: func() error { t.Fatal("repeated catalog commit"); return nil }}
	if err := tx.Apply(hooks); err == nil {
		t.Fatal("write failure missing")
	}
	current, _ := os.ReadFile(filepath.Join(root, "integration"))
	if string(current) != "new" {
		t.Fatal("committed files rolled back")
	}
	if _, err := os.Stat(filepath.Join(root, journalName)); err != nil {
		t.Fatal("resume journal discarded")
	}
	tx.write = deployment.AtomicWrite
	if err := tx.Apply(hooks); err != nil {
		t.Fatal("forward retry failed", err)
	}
}

func TestCompatibleTupleRestoreAllowsSubsequentSetup(t *testing.T) {
	backend, home, r := fixture(t)
	ctx := context.Background()
	beforeRelease := deployment.Release
	t.Cleanup(func() { deployment.Release = beforeRelease })
	deployment.Release = "0.9.0"
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	original, _ := ReadCatalog(ctx, r.Profile.StatePath)
	r.ExpectedRevision = original.Revision
	deployment.Release = "1.0.0"
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, ".config/gpu-workload-supervisor")
	entries, err := os.ReadDir(filepath.Join(root, "backups"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("%v %v", entries, err)
	}
	backup := filepath.Join(root, "backups", entries[0].Name())
	restore := func(source, destination string) {
		t.Helper()
		data, err := privateRead(filepath.Join(backup, source))
		if err != nil {
			t.Fatal(err)
		}
		if err := deployment.AtomicWrite(destination, data); err != nil {
			t.Fatal(err)
		}
	}
	// All state users are closed. Replace the complete compatible tuple, including
	// setup ownership and retained binary metadata; never edit migration versions.
	os.Remove(r.Profile.StatePath + "-wal")
	os.Remove(r.Profile.StatePath + "-shm")
	restore("state.db", r.Profile.StatePath)
	restore("state.db"+deployment.Suffix, r.Profile.StatePath+deployment.Suffix)
	for _, name := range []string{"operator.json", "catalog.json", manifestName} {
		restore(name, filepath.Join(root, name))
	}
	for _, name := range append(append([]string(nil), binaries...), "manifest.json") {
		restore(name, filepath.Join(root, "activated-binaries", name))
	}
	os.Remove(filepath.Join(root, journalName))
	os.Remove(filepath.Join(root, "activation.json"))
	deployment.Release = "0.9.0"
	restored, err := store.OpenRestored(ctx, r.Profile.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restored.RotateIncarnation(ctx); err != nil {
		t.Fatal(err)
	}
	restored.Close()
	if err := backend.Reconcile(ctx, home); err != nil {
		t.Fatal(err)
	}
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal("matching restored ownership rejected", err)
	}
}
