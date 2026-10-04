package setup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
)

func TestAcceptanceRemovalRefusesUntrustedOwnership(t *testing.T) {
	for _, kind := range []string{"missing-root", "public-record", "malformed-record", "wrong-version", "wrong-unit", "wrong-target", "symlink-parent", "regular-link"} {
		t.Run(kind, func(t *testing.T) {
			home := t.TempDir()
			root := filepath.Join(home, ".config/gpu-workload-supervisor")
			record := filepath.Join(root, "integration.json")
			wants := filepath.Join(home, ".config/systemd/user/default.target.wants")
			owned := integration{1, reconcileUnit, "/usr/lib/systemd/user/" + reconcileUnit}
			if kind == "missing-root" {
				if err := RemoveIntegration(home); err == nil {
					t.Fatal("missing ownership root accepted")
				}
				return
			}
			switch kind {
			case "wrong-version":
				owned.Version = 2
			case "wrong-unit":
				owned.Unit = "user-workload.service"
			case "wrong-target":
				owned.Target = "/tmp/unowned"
			}
			acceptanceJSON(t, record, owned)
			if kind == "public-record" {
				os.Chmod(record, 0644)
			}
			if kind == "malformed-record" {
				acceptanceWrite(t, record, []byte("{"))
			}
			if kind == "symlink-parent" {
				parent := filepath.Dir(wants)
				if err := os.MkdirAll(parent, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), wants); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.MkdirAll(wants, 0700); err != nil {
					t.Fatal(err)
				}
			}
			link := filepath.Join(wants, reconcileUnit)
			if kind == "regular-link" {
				acceptanceWrite(t, link, []byte("user-owned unit"))
			}
			if err := RemoveIntegration(home); err == nil {
				t.Fatal("unsafe removal accepted")
			}
			if _, err := os.Lstat(record); err != nil {
				t.Fatal("ownership evidence lost", err)
			}
			if kind == "regular-link" {
				data, err := os.ReadFile(link)
				if err != nil || string(data) != "user-owned unit" {
					t.Fatal("user unit modified", err)
				}
			}
		})
	}
}

func TestAcceptanceEnablePreservesUserDirectoryAndRecordConflicts(t *testing.T) {
	for _, kind := range []string{"blocked-user-dir", "blocked-wants", "blocked-record"} {
		t.Run(kind, func(t *testing.T) {
			home, _ := fixture(t)
			root := filepath.Join(home, ".config/gpu-workload-supervisor")
			if err := os.MkdirAll(root, 0700); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "blocked-user-dir":
				acceptanceWrite(t, filepath.Join(home, ".config/systemd/user"), []byte("user file"))
			case "blocked-wants":
				acceptanceWrite(t, filepath.Join(home, ".config/systemd/user/default.target.wants"), []byte("user file"))
			case "blocked-record":
				if err := os.Mkdir(filepath.Join(root, "integration.json"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			runCommand = func(context.Context, string, ...string) ([]byte, error) {
				t.Fatal("command ran before filesystem validation")
				return nil, nil
			}
			if err := enableReconciliation(context.Background(), home, "/usr/bin/systemctl"); err == nil {
				t.Fatal("unsafe enable accepted")
			}
		})
	}
}

func TestAcceptanceBackupTupleRejectsMissingOrMismatchedMembers(t *testing.T) {
	for _, kind := range []string{"malformed-manifest", "malformed-profile", "release-mismatch", "missing-marker", "missing-catalog", "missing-binary", "public-destination", "blocked-destination", "invalid-catalog"} {
		t.Run(kind, func(t *testing.T) {
			home, r := fixture(t)
			if err := Apply(context.Background(), home, r); err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(home, ".config/gpu-workload-supervisor")
			profile, err := os.ReadFile(filepath.Join(root, "operator.json"))
			if err != nil {
				t.Fatal(err)
			}
			dest := t.TempDir()
			switch kind {
			case "malformed-manifest":
				acceptanceWrite(t, filepath.Join(root, "activated-binaries/manifest.json"), []byte("{"))
			case "malformed-profile":
				profile = []byte("{")
			case "release-mismatch":
				var p Profile
				if err := json.Unmarshal(profile, &p); err != nil {
					t.Fatal(err)
				}
				p.ActivatedRelease = "other"
				profile, _ = json.Marshal(p)
			case "missing-marker":
				os.Remove(r.Profile.StatePath + deployment.Suffix)
			case "missing-catalog":
				os.Remove(filepath.Join(root, "catalog.json"))
			case "missing-binary":
				os.Remove(filepath.Join(root, "activated-binaries/gpu-mode"))
			case "public-destination":
				for _, name := range append(append([]string{}, binaries...), "operator.json", "manifest.json", "catalog.json", "state.db"+deployment.Suffix) {
					acceptanceWrite(t, filepath.Join(dest, name), []byte("untrusted"))
					os.Chmod(filepath.Join(dest, name), 0644)
				}
			case "blocked-destination":
				dest = filepath.Join(dest, "missing")
			case "invalid-catalog":
				acceptanceWrite(t, filepath.Join(root, "catalog.json"), []byte("{"))
			}
			err = copyActivation(root, dest, profile)
			if kind == "invalid-catalog" || kind == "missing-catalog" { // The durable DB catalog is authoritative, so a stale mirror cannot corrupt the tuple.
				if err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(filepath.Join(dest, "catalog.json"))
				if err != nil || !json.Valid(data) {
					t.Fatal("backup copied invalid mirror", err)
				}
			} else if err == nil {
				t.Fatal("unsafe binary/config tuple accepted")
			}
		})
	}
}

func TestAcceptanceInterruptedActivationRequiresReadableOriginalPlan(t *testing.T) {
	for _, kind := range []string{"missing", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			home, r := fixture(t)
			ctx := context.Background()
			runCommand = func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("interrupted enable") }
			if err := Apply(ctx, home, r); err == nil {
				t.Fatal("missing injected failure")
			}
			path := filepath.Join(home, ".config/gpu-workload-supervisor/activation.json")
			if kind == "missing" {
				os.Remove(path)
			} else {
				acceptanceWrite(t, path, []byte("{"))
			}
			if err := Apply(ctx, home, r); err == nil {
				t.Fatal("resumed without original activation")
			}
			marker, err := deployment.Read(r.Profile.StatePath)
			if err != nil || !marker.Maintenance {
				t.Fatal("lost recovery fence", err)
			}
		})
	}
}
