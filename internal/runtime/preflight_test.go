package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCapabilityPreflight(t *testing.T) {
	for _, scenario := range []string{"populated", "removed stopped", "missing active", "missing anchor", "unsupported", "wrong path"} {
		t.Run(scenario, func(t *testing.T) {
			r := stoppedRunner()
			m := strictManager(t, r)
			root := fixtureCgroups(t, m)
			r.outputs[textShowCommand] = []byte("LoadState=loaded\nActiveState=active\nSubState=running\nControlGroup=/workloads/text.service\n")
			writeEvents(t, root, "text.service", "populated 1\n")
			switch scenario {
			case "removed stopped":
				r.outputs[textShowCommand] = []byte("LoadState=loaded\nActiveState=inactive\nSubState=dead\nControlGroup=/workloads/text.service\n")
				os.RemoveAll(filepath.Join(root, "text.service"))
			case "missing active":
				os.RemoveAll(filepath.Join(root, "text.service"))
			case "missing anchor":
				os.Remove(filepath.Join(root, "cgroup.events"))
			case "unsupported":
				m.cgroups.verify = func(int) error { return errors.New("unsupported hierarchy") }
			case "wrong path":
				r.outputs[textShowCommand] = []byte("LoadState=loaded\nActiveState=active\nSubState=running\nControlGroup=/elsewhere\n")
			}
			err := m.Preflight(context.Background())
			if (err == nil) != (scenario == "populated" || scenario == "removed stopped") {
				t.Fatalf("preflight %v", err)
			}
		})
	}
}

func TestPreflightChecksConfiguredStoppedCgroupWithBlankMetadata(t *testing.T) {
	for _, evidence := range []string{"removed", "populated 1\n", "populated 0\n", "malformed", "missing events"} {
		t.Run(evidence, func(t *testing.T) {
			r := stoppedRunner()
			m := strictManager(t, r)
			root := fixtureCgroups(t, m)
			if evidence != "removed" {
				writeEvents(t, root, "media.service", evidence)
			}
			if evidence == "missing events" {
				if err := os.Remove(filepath.Join(root, "media.service", "cgroup.events")); err != nil {
					t.Fatal(err)
				}
			}
			err := m.Preflight(context.Background())
			wantError := evidence == "malformed" || evidence == "missing events"
			if (err != nil) != wantError {
				t.Fatalf("preflight %q: %v", evidence, err)
			}
		})
	}
}

func TestPreflightRemovedCgroupsRespectUnitState(t *testing.T) {
	for _, tc := range []struct {
		active, sub string
		allowed     bool
	}{
		{"failed", "failed", true},
		{"inactive", "dead", true},
		{"active", "running", false},
		{"activating", "start", false},
		{"deactivating", "stop", false},
		{"unknown", "unknown", false},
	} {
		t.Run(tc.active, func(t *testing.T) {
			r := stoppedRunner()
			m := strictManager(t, r)
			fixtureCgroups(t, m)
			r.outputs[textShowCommand] = []byte("LoadState=loaded\nActiveState=" + tc.active + "\nSubState=" + tc.sub + "\nControlGroup=\n")
			err := m.Preflight(context.Background())
			if (err == nil) != tc.allowed {
				t.Fatalf("preflight %s/%s: %v", tc.active, tc.sub, err)
			}
			if tc.active == "failed" && m.Released(context.Background()) == nil {
				t.Fatal("failed unit treated as release evidence")
			}
		})
	}
}
