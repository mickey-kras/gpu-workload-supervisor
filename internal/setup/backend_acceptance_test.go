package setup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

func acceptanceWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
func acceptanceJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	acceptanceWrite(t, path, data)
}

func TestAcceptanceActivationRejectsUnsafeInputsBeforeMaintenance(t *testing.T) {
	for _, kind := range []string{"invalid-request", "blocked-config", "blocked-state", "malformed-profile", "public-profile", "relocated-state", "proxy-held", "malformed-marker", "marker-mismatch", "runtime-construction", "bad-activation-path"} {
		t.Run(kind, func(t *testing.T) {
			backend, home, r := fixture(t)
			ctx := context.Background()
			root := filepath.Join(home, ".config/gpu-workload-supervisor")
			switch kind {
			case "invalid-request":
				r.Version = 2
			case "blocked-config":
				acceptanceWrite(t, filepath.Join(home, ".config"), []byte("user file"))
			case "blocked-state":
				acceptanceWrite(t, filepath.Dir(r.Profile.StatePath), []byte("user file"))
			case "malformed-profile":
				acceptanceWrite(t, filepath.Join(root, "operator.json"), []byte("{"))
			case "public-profile":
				acceptanceJSON(t, filepath.Join(root, "operator.json"), r.Profile)
				os.Chmod(filepath.Join(root, "operator.json"), 0644)
			case "relocated-state":
				old := r.Profile
				old.StatePath += "-previous"
				acceptanceJSON(t, filepath.Join(root, "operator.json"), old)
			case "proxy-held":
				gate, err := lock.TryAcquire(r.Profile.StatePath + ".proxy.lock")
				if err != nil {
					t.Fatal(err)
				}
				defer gate.Close()
			case "malformed-marker":
				acceptanceWrite(t, r.Profile.StatePath+deployment.Suffix, []byte("{"))
			case "marker-mismatch":
				old := r.Profile
				old.ActivatedRelease = "0.1.0"
				acceptanceJSON(t, filepath.Join(root, "operator.json"), old)
			case "runtime-construction":
				backend.makeRuntime = func(Request) (gpuruntime.Manager, error) { return nil, errors.New("invalid executable") }
			case "bad-activation-path":
				if err := os.MkdirAll(filepath.Join(root, "activation.json"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := backend.Apply(ctx, home, r); err == nil {
				t.Fatal("unsafe activation accepted")
			}
			if kind != "malformed-marker" {
				if marker, err := deployment.Read(r.Profile.StatePath); err == nil && marker.Maintenance {
					t.Fatal("failure fenced healthy deployment before validation")
				}
			}
		})
	}
}

func TestAcceptanceExistingActivationRejectsUnsafeUpgrade(t *testing.T) {
	for _, kind := range []string{"unhealthy-state", "missing-catalog-table-columns", "old-runtime-invalid", "old-runtime-busy", "blocked-backups", "missing-binary-tuple"} {
		t.Run(kind, func(t *testing.T) {
			backend, home, r := fixture(t)
			ctx := context.Background()
			if err := backend.Apply(ctx, home, r); err != nil {
				t.Fatal(err)
			}
			snapshot, err := ReadCatalog(ctx, r.Profile.StatePath)
			if err != nil {
				t.Fatal(err)
			}
			r.ExpectedRevision = snapshot.Revision
			root := filepath.Join(home, ".config/gpu-workload-supervisor")
			switch kind {
			case "unhealthy-state", "missing-catalog-table-columns":
				db, err := sql.Open("sqlite", r.Profile.StatePath)
				if err != nil {
					t.Fatal(err)
				}
				query := "UPDATE control_state SET admission='open'"
				if kind == "missing-catalog-table-columns" {
					query = "ALTER TABLE workload_catalog RENAME COLUMN catalog TO broken"
				}
				if _, err := db.Exec(query); err != nil {
					t.Fatal(err)
				}
				db.Close()
			case "old-runtime-invalid", "old-runtime-busy":
				calls := 0
				backend.makeRuntime = func(Request) (gpuruntime.Manager, error) {
					calls++
					if calls == 2 {
						if kind == "old-runtime-invalid" {
							return nil, errors.New("previous executable invalid")
						}
						return idleRuntime{err: errors.New("old mapping still live")}, nil
					}
					return idleRuntime{}, nil
				}
			case "blocked-backups":
				acceptanceWrite(t, filepath.Join(root, "backups"), []byte("user file"))
			case "missing-binary-tuple":
				if err := os.Remove(filepath.Join(root, "activated-binaries/manifest.json")); err != nil {
					t.Fatal(err)
				}
			}
			if err := backend.Apply(ctx, home, r); err == nil {
				t.Fatal("unsafe upgrade accepted")
			}
			marker, err := deployment.Read(r.Profile.StatePath)
			if err != nil || marker.Maintenance {
				t.Fatalf("preflight failure wedged managed state: %+v %v", marker, err)
			}
		})
	}
}

func TestAcceptanceDiscoveryAndReconciliationRejectDamagedProfiles(t *testing.T) {
	for _, kind := range []string{"missing", "malformed", "public", "missing-state", "missing-managed-state", "corrupt-state", "held-gate", "maintenance", "runtime-invalid"} {
		t.Run(kind, func(t *testing.T) {
			backend, home, r := fixture(t)
			ctx := context.Background()
			root := filepath.Join(home, ".config/gpu-workload-supervisor")
			if kind != "missing" && kind != "malformed" && kind != "public" && kind != "missing-state" {
				if err := backend.Apply(ctx, home, r); err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "malformed":
				acceptanceWrite(t, filepath.Join(root, "operator.json"), []byte("{"))
			case "public":
				acceptanceJSON(t, filepath.Join(root, "operator.json"), r.Profile)
				os.Chmod(filepath.Join(root, "operator.json"), 0644)
			case "missing-state":
				acceptanceJSON(t, filepath.Join(root, "operator.json"), r.Profile)
			case "missing-managed-state":
				if err := os.Remove(r.Profile.StatePath); err != nil {
					t.Fatal(err)
				}
			case "corrupt-state":
				acceptanceWrite(t, r.Profile.StatePath, []byte("not sqlite"))
			case "held-gate":
				gate, err := lock.TryAcquire(r.Profile.StatePath + ".lock")
				if err != nil {
					t.Fatal(err)
				}
				defer gate.Close()
			case "maintenance":
				if err := deployment.Write(r.Profile.StatePath, deployment.Marker{Version: 1, Release: deployment.Release, Maintenance: true}); err != nil {
					t.Fatal(err)
				}
			case "runtime-invalid":
				backend.makeRuntime = func(Request) (gpuruntime.Manager, error) { return nil, errors.New("invalid runtime") }
			}
			if err := backend.Reconcile(ctx, home); err == nil {
				t.Fatal("unsafe reconciliation accepted")
			}
			if kind == "missing-state" || kind == "missing-managed-state" {
				if _, err := os.Stat(r.Profile.StatePath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("reconciliation recreated missing managed database: %v", err)
				}
			}
			if kind == "malformed" || kind == "public" || kind == "corrupt-state" {
				if _, err := backend.Discover(ctx, home); err == nil {
					t.Fatal("damaged configured state discovered successfully")
				}
			}
		})
	}
}

func TestAcceptanceDiscoveryFailures(t *testing.T) {
	for _, kind := range []string{"malformed-activation", "malformed-marker", "command-failure", "oversized-output"} {
		t.Run(kind, func(t *testing.T) {
			backend, home, r := fixture(t)
			root := filepath.Join(home, ".config/gpu-workload-supervisor")
			switch kind {
			case "malformed-activation":
				acceptanceWrite(t, filepath.Join(root, "activation.json"), []byte("{"))
			case "malformed-marker":
				acceptanceJSON(t, filepath.Join(root, "activation.json"), activation{Request: r})
				acceptanceWrite(t, r.Profile.StatePath+deployment.Suffix, []byte("{"))
			case "command-failure":
				backend.runCommand = func(context.Context, string, ...string) ([]byte, error) {
					return nil, errors.New("user bus unavailable")
				}
			case "oversized-output":
				backend.runCommand = func(context.Context, string, ...string) ([]byte, error) {
					return []byte(strings.Repeat("x", 1048577)), nil
				}
			}
			if _, err := backend.Discover(context.Background(), home); err == nil {
				t.Fatal("discovery failure ignored")
			}
		})
	}
}
