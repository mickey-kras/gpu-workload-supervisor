package runtime

import (
	"context"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"net/http"
	"testing"
)

func TestCatalogObservesThirdRuntime(t *testing.T) {
	cfg := testConfig()
	c := control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "speech", Label: "Speech", Adapter: "systemd", Unit: "speech.service", Cgroup: "/user/speech", HealthURL: "http://localhost:9000"}}}
	cfg.Catalog = &c
	r := &fakeRunner{outputs: map[string][]byte{"/usr/bin/true --user show --property=LoadState --property=ActiveState --property=SubState --property=ControlGroup -- speech.service": []byte("LoadState=loaded\nActiveState=active\nSubState=running\nControlGroup=/user/speech\n")}}
	m, err := newSystemdManager(cfg, r, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := m.Observe(context.Background())
	if err != nil || !snap.Workloads["speech"].Active {
		t.Fatalf("%+v %v", snap, err)
	}
}

func TestReleaseUnchangedLaunchesKeepsRemovedWorkloadEvidence(t *testing.T) {
	runner := stoppedRunner()
	manager := strictManager(t, runner)
	root := fixtureCgroups(t, manager)
	writeEvents(t, root, "text.service", "populated 0\n")
	writeEvents(t, root, "media.service", "populated 0\n")
	// Setup already proved this accepted binding before replacing its source.
	// Its obsolete source must not be read again; the candidate checks its new
	// binding. The other accepted workload is still checked, even if removed.
	manager.config.Catalog.Profiles[0].LaunchBinding = &control.LaunchBinding{
		LaunchFile: "/units/text.service",
		GPUUUID:    "GPU-01234567-89ab-cdef-0123-456789abcdef",
	}
	ctx := context.Background()
	if err := manager.ReleasedForUnchangedLaunches(ctx, control.WorkloadIdle, map[string]bool{"/units/text.service-other": true}); err == nil {
		t.Fatal("inexact source replacement bypassed the accepted binding")
	}
	replaced := map[string]bool{"/units/text.service": true}
	if err := manager.ReleasedForUnchangedLaunches(ctx, control.WorkloadIdle, replaced); err != nil {
		t.Fatal(err)
	}
	runner.outputs[mediaShowCommand] = []byte("LoadState=loaded\nActiveState=active\nSubState=running\nControlGroup=/workloads/media.service\n")
	if err := manager.ReleasedForUnchangedLaunches(ctx, control.WorkloadIdle, replaced); err == nil {
		t.Fatal("removed accepted workload was running")
	}
	runner.outputs[mediaShowCommand] = stoppedOutput()
	writeEvents(t, root, "media.service", "populated 1\n")
	if err := manager.ReleasedForUnchangedLaunches(ctx, control.WorkloadIdle, replaced); err == nil {
		t.Fatal("removed accepted workload retained a descendant")
	}
}
