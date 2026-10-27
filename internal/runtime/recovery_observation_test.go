package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestRecoveryObservationAllowsOnlyFailedUnitsBeyondNormalStates(t *testing.T) {
	for _, state := range []struct{ active, sub string }{
		{"failed", "failed"}, {"failed", "running"}, {"active", "exited"},
		{"activating", "start"}, {"deactivating", "stop"}, {"unknown", "unknown"},
	} {
		t.Run(state.active+"/"+state.sub, func(t *testing.T) {
			r := stoppedRunner()
			m := strictManager(t, r)
			r.outputs[textShowCommand] = []byte("LoadState=loaded\nActiveState=" + state.active + "\nSubState=" + state.sub + "\nControlGroup=\n")
			if _, err := m.Observe(context.Background()); err == nil {
				t.Fatal("normal observation accepted non-running/non-stopped unit")
			}
			snap, err := m.ObserveForRecovery(context.Background())
			if state.active != "failed" || state.sub != "failed" {
				if err == nil {
					t.Fatal("recovery accepted ambiguous unit")
				}
				return
			}
			if err != nil || snap.Workloads[control.WorkloadText].Active {
				t.Fatalf("failed unit remains retainable: %#v %v", snap, err)
			}
			if m.ReleasedFor(context.Background(), control.WorkloadIdle) == nil {
				t.Fatal("failed unit counted as released")
			}
		})
	}
}

func TestRecoveryObservedFailureRequiresSafeStopAndIndependentRelease(t *testing.T) {
	for _, adapter := range []string{"systemd", control.AdapterMediaUnload} {
		for _, scenario := range []string{"removed", "empty", "populated", "missing metadata", "reset failure", "still failed"} {
			t.Run(adapter+"/"+scenario, func(t *testing.T) {
				ctx := context.Background()
				r := &failedStopRunner{fakeRunner: *stoppedRunner()}
				r.outputs[textShowCommand] = []byte("LoadState=loaded\nActiveState=failed\nSubState=failed\nControlGroup=\n")
				m := strictManager(t, r)
				// Exercise Stop's media adapter branch on the actual failed unit.
				m.config.Catalog.Profiles[0].Adapter = adapter
				root := fixtureCgroups(t, m)
				switch scenario {
				case "empty":
					writeEvents(t, root, "text.service", "populated 0\n")
				case "populated":
					writeEvents(t, root, "text.service", "populated 1\n")
				case "missing metadata":
					r.outputs[textShowCommand] = []byte("LoadState=loaded\nActiveState=failed\nSubState=failed\n")
				case "reset failure":
					r.resetError = errors.New("reset rejected")
				case "still failed":
					r.remainFailed = true
				}
				if err := m.Preflight(ctx); err != nil {
					if scenario == "missing metadata" {
						return
					}
					t.Fatal(err)
				}
				if scenario == "missing metadata" {
					t.Fatal("preflight accepted missing metadata")
				}
				snap, err := m.ObserveForRecovery(ctx)
				if err != nil || snap.AnyActive() {
					t.Fatalf("recovery observation: %#v %v", snap, err)
				}
				if m.ReleasedFor(ctx, control.WorkloadIdle) == nil {
					t.Fatal("observation bypassed release")
				}
				err = m.Stop(ctx, control.WorkloadText)
				wantSuccess := scenario == "removed" || scenario == "empty"
				if (err == nil) != wantSuccess {
					t.Fatalf("failed unit cleanup: %v", err)
				}
				if (m.ReleasedFor(ctx, control.WorkloadIdle) == nil) != wantSuccess {
					t.Fatal("release accepted unsafe cleanup or rejected safe cleanup")
				}
				if scenario == "populated" {
					for _, call := range r.calls {
						if call == "/usr/bin/true --user reset-failed -- text.service" {
							t.Fatal("populated group reset")
						}
					}
				}
			})
		}
	}
}
