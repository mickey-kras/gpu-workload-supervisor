package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
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
			if tc.active == "failed" && m.ReleasedFor(context.Background(), control.WorkloadIdle) == nil {
				t.Fatal("failed unit treated as release evidence")
			}
		})
	}
}

type failedStopRunner struct {
	fakeRunner
	resetError   error
	remainFailed bool
}

func (r *failedStopRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if len(args) > 1 && args[1] == "reset-failed" {
		if r.resetError != nil {
			return nil, r.resetError
		}
		if !r.remainFailed {
			r.outputs[textShowCommand] = stoppedOutput()
		}
	}
	return r.fakeRunner.Run(ctx, name, args...)
}

func TestStopFailedUnitResetsOnlyAfterVerifiedEmptyCgroup(t *testing.T) {
	for _, adapter := range []string{"systemd", control.AdapterMediaUnload} {
		for _, scenario := range []string{"removed", "empty", "populated", "missing events", "missing metadata", "reset failure", "still failed"} {
			t.Run(adapter+"/"+scenario, func(t *testing.T) {
				r := &failedStopRunner{fakeRunner: fakeRunner{outputs: map[string][]byte{
					textShowCommand:  []byte("LoadState=loaded\nActiveState=failed\nSubState=failed\nControlGroup=\n"),
					mediaShowCommand: stoppedOutput(),
				}}}
				m := strictManager(t, r)
				m.config.Catalog.Profiles[1].Adapter = adapter
				if adapter == control.AdapterMediaUnload {
					m.config.Catalog.Profiles[1].ReleaseURL = "http://127.0.0.1:8188/free"
				}
				root := fixtureCgroups(t, m)
				switch scenario {
				case "empty":
					writeEvents(t, root, "text.service", "populated 0\n")
				case "populated":
					writeEvents(t, root, "text.service", "populated 1\n")
				case "missing events":
					writeEvents(t, root, "text.service", "populated 0\n")
					if err := os.Remove(filepath.Join(root, "text.service", "cgroup.events")); err != nil {
						t.Fatal(err)
					}
				case "missing metadata":
					r.outputs[textShowCommand] = []byte("LoadState=loaded\nActiveState=failed\nSubState=failed\n")
				case "reset failure":
					r.resetError = errors.New("reset rejected")
				case "still failed":
					r.remainFailed = true
				}
				err := m.StopForRecovery(context.Background())
				wantSuccess := scenario == "removed" || scenario == "empty"
				if (err == nil) != wantSuccess {
					t.Fatalf("stop recovery: %v", err)
				}
				reset := false
				for _, call := range r.calls {
					if call == "/usr/bin/true --user reset-failed -- text.service" {
						reset = true
					}
				}
				if (scenario == "populated" || scenario == "missing events" || scenario == "missing metadata") && reset {
					t.Fatal("reset failure before verifying empty group")
				}
				if wantSuccess && m.ReleasedFor(context.Background(), control.WorkloadIdle) != nil {
					t.Fatal("recovered stopped unit failed release")
				}
			})
		}
	}
}
