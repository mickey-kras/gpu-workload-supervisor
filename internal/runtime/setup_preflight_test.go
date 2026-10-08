package runtime

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestPreviewPreflightChecksAdoptedBindingsWithPendingOwnedLaunch(t *testing.T) {
	binaryDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binaryDir, "ollama"), []byte("fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	previous := ownedBinaryDirectory
	ownedBinaryDirectory = binaryDir
	t.Cleanup(func() { ownedBinaryDirectory = previous })
	for _, failure := range []string{"none", "missing launch", "drifted launch", "missing unit", "missing cgroup"} {
		t.Run(failure, func(t *testing.T) {
			m, r, adopted, fragmentCommand := nativeFixture(t, "llama.cpp", "http://localhost:9000")
			m.config.OwnedUnitDir = t.TempDir()
			owned := ownedRenderProfile("ollama", "chat", &control.OwnedLaunch{Port: 11434})
			owned.Label, owned.Adapter, owned.HealthURL = "Chat", "systemd", "http://127.0.0.1:11434/api/tags"
			owned.Cgroup = OwnedCgroup("/workloads", owned)
			raw, err := RenderOwnedUnit(owned)
			if err != nil {
				t.Fatal(err)
			}
			owned.NativeModel.LaunchSHA256 = fmt.Sprintf("%x", sha256.Sum256(raw))
			m.config.Catalog.Version = 2
			m.config.Catalog.Profiles = append(m.config.Catalog.Profiles, owned)
			switch failure {
			case "missing launch":
				if err := os.Remove(adopted.NativeModel.LaunchFile); err != nil {
					t.Fatal(err)
				}
			case "drifted launch":
				if err := os.WriteFile(adopted.NativeModel.LaunchFile, []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing unit":
				r.outputs[fragmentCommand] = []byte("FragmentPath=\nDropInPaths=\nNeedDaemonReload=no\n")
			case "missing cgroup":
				r.outputs[catalogShow("speech")] = []byte("LoadState=loaded\nActiveState=active\nSubState=running\nControlGroup=/workloads/speech.service\n")
			}
			err = m.PreflightAdopted(t.Context(), nil)
			if (err != nil) != (failure != "none") {
				t.Fatalf("preview preflight %q: %v", failure, err)
			}
			for _, call := range r.calls {
				if strings.Contains(call, " start ") || strings.Contains(call, " stop ") {
					t.Fatalf("preview changed runtime: %s", call)
				}
				if strings.Contains(call, "-- "+owned.Unit) {
					t.Fatalf("preview required pending owned unit: %s", call)
				}
			}
		})
	}
}

func TestActivationPreflightAllowsOnlyProvenPendingOwnedRemoval(t *testing.T) {
	r := stoppedRunner()
	m := strictManager(t, r)
	fixtureCgroups(t, m)
	dir := t.TempDir()
	m.config.OwnedUnitDir = dir
	name := "gws-owned-old.service"
	path := filepath.Join(dir, name)
	raw := []byte("accepted contents")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	proof := fmt.Sprintf("%x", sha256.Sum256(raw))
	if err := m.Preflight(t.Context()); !errors.Is(err, ErrOrphanedOwnedUnit) {
		t.Fatalf("ordinary preflight allowed orphan: %v", err)
	}
	if err := m.PreflightWithOwnedRemovals(t.Context(), map[string]string{name: proof}); err != nil {
		t.Fatalf("accepted pending removal rejected: %v", err)
	}
	if err := os.WriteFile(path, []byte("foreign change"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.PreflightWithOwnedRemovals(t.Context(), map[string]string{name: proof}); !errors.Is(err, ErrOrphanedOwnedUnit) {
		t.Fatalf("drifted pending removal accepted: %v", err)
	}
}
