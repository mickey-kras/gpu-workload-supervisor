package setup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestActivationBackupRestoresRemovedOwnedUnit(t *testing.T) {
	backend, home, request := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	profile, _ := ownedFixtureProfile(t, home, "vision", 9100)
	request.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}
	ctx := context.Background()
	if err := backend.Apply(ctx, home, request); err != nil {
		t.Fatal(err)
	}
	// Backstops are prepared during activation; use the committed fingerprint.
	snapshot, err := ReadCatalog(ctx, request.Profile.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	profile = snapshot.Catalog.Profiles[0]
	request.ExpectedRevision = snapshot.Revision
	request.Catalog = control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "text", Label: "Text", Adapter: "systemd", Unit: "text.service", Cgroup: "/user.slice/text", HealthURL: "http://127.0.0.1:8000/health"}}}
	if err := backend.Apply(ctx, home, request); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, ".config/gpu-workload-supervisor")
	if _, present, err := readOwnedUnitJournal(root); err != nil || present {
		t.Fatalf("successful removal retained journal: %v %v", present, err)
	}
	if _, err := os.Lstat(profile.NativeModel.LaunchFile); !os.IsNotExist(err) {
		t.Fatalf("unit was not removed: %v", err)
	}
	backups, err := filepath.Glob(filepath.Join(root, "backups/activation-*"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("expected one rollback tuple: %v %v", backups, err)
	}
	directory := backups[0]
	data, err := privateRead(filepath.Join(directory, "owned-units.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest ownedUnitBackupManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Version != 1 || len(manifest.Hashes) != 1 || manifest.Hashes[profile.Unit] != profile.NativeModel.LaunchSHA256 {
		t.Fatalf("wrong unit manifest: %+v", manifest)
	}
	restoredCatalog, err := ReadCatalog(ctx, filepath.Join(directory, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if restoredCatalog.Catalog.Profiles[0].NativeModel.LaunchSHA256 != manifest.Hashes[profile.Unit] {
		t.Fatal("backup catalog and unit manifest disagree")
	}
	raw, err := privateRead(filepath.Join(directory, "owned-units", profile.Unit))
	if err != nil || digest(raw) != manifest.Hashes[profile.Unit] {
		t.Fatalf("backup lost removed unit bytes: %v", err)
	}
	// Reinstall the saved bytes in the now-absent path, as in the documented
	// stopped-runtime restore procedure, and verify the restored binding.
	if err := os.WriteFile(profile.NativeModel.LaunchFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyOwnedUnitRenderings(home, restoredCatalog.Catalog); err != nil {
		t.Fatal(err)
	}
}

func TestActivationUnitBackupUsesVerifiedPrewriteBytes(t *testing.T) {
	home := t.TempDir()
	profile, raw := ownedFixtureProfile(t, home, "vision", 9100)
	catalog := control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}
	path := profile.NativeModel.LaunchFile
	if err := mkdirTrusted(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	// The active candidate has already replaced the accepted rendering.
	if err := os.WriteFile(path, []byte("new candidate"), 0600); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	plan := unitPlan{prior: map[string][]byte{profile.Unit: raw}}
	if err := collectActivationUnits(home, catalog, plan, files); err != nil {
		t.Fatal(err)
	}
	if digest(files[filepath.Join("owned-units", profile.Unit)]) != profile.NativeModel.LaunchSHA256 {
		t.Fatal("backed up the new candidate instead of the prior rendering")
	}
	plan.prior[profile.Unit] = []byte("foreign prior")
	if err := collectActivationUnits(home, catalog, plan, map[string][]byte{}); err == nil {
		t.Fatal("accepted an unverified prewrite snapshot")
	}
}

func TestActivationUnitBackupRejectsUntrustedOrModifiedFiles(t *testing.T) {
	for _, kind := range []string{"modified", "missing", "symlink", "writable"} {
		t.Run(kind, func(t *testing.T) {
			home := t.TempDir()
			profile, raw := ownedFixtureProfile(t, home, "vision", 9100)
			if err := mkdirTrusted(ownedUnitDirectory(home)); err != nil {
				t.Fatal(err)
			}
			path := profile.NativeModel.LaunchFile
			switch kind {
			case "modified":
				err := os.WriteFile(path, []byte("foreign content"), 0600)
				if err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := filepath.Join(home, "target")
				if err := os.WriteFile(target, raw, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case "writable":
				if err := os.WriteFile(path, raw, 0666); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0666); err != nil {
					t.Fatal(err)
				}
			}
			catalog := control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}
			if err := collectActivationUnits(home, catalog, unitPlan{}, map[string][]byte{}); err == nil {
				t.Fatal("accepted unsafe unit backup")
			}
		})
	}
}
