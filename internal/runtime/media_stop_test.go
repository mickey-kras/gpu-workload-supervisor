package runtime

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func stoppedOutput() []byte {
	return []byte("LoadState=loaded\nActiveState=inactive\nSubState=dead\nControlGroup=\n")
}

func strictManager(t *testing.T, runner CommandRunner) *SystemdManager {
	t.Helper()
	config := testConfig()
	config.Catalog.Profiles[1].Adapter = "systemd"
	config.Catalog.Profiles[1].ReleaseURL = ""
	manager, err := newSystemdManager(config, runner, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	fixtureCgroups(t, manager)
	return manager
}

func TestStopServiceStopsMediaAndVerifiesCompletion(t *testing.T) {
	runner := &recoveryRunner{fakeRunner: fakeRunner{outputs: map[string][]byte{}}}
	manager := strictManager(t, runner)
	if err := manager.Stop(context.Background(), control.WorkloadMedia); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 || runner.calls[0] != "/usr/bin/true --user stop -- media.service" || runner.calls[1] != mediaShowCommand {
		t.Fatalf("calls = %v", runner.calls)
	}
}

func TestStopServiceRejectsUnverifiedShutdownAndObservation(t *testing.T) {
	for _, state := range []string{"active/running", "active/exited", "activating/start", "deactivating/stop-sigterm", "failed/failed", "inactive/running", "unknown/dead"} {
		t.Run(state, func(t *testing.T) {
			parts := strings.Split(state, "/")
			runner := &fakeRunner{outputs: map[string][]byte{textShowCommand: stoppedOutput(), mediaShowCommand: []byte("LoadState=loaded\nActiveState=" + parts[0] + "\nSubState=" + parts[1] + "\n")}}
			manager := strictManager(t, runner)
			if err := manager.Stop(context.Background(), control.WorkloadMedia); err == nil {
				t.Fatal("accepted incomplete stop")
			}
			if _, err := manager.Observe(context.Background()); err == nil && state != "active/running" {
				t.Fatal("accepted ambiguous observation")
			}
			if err := manager.ReleasedFor(context.Background(), control.WorkloadIdle); err == nil {
				t.Fatal("accepted release with media not stopped")
			}
			if err := manager.Start(context.Background(), control.WorkloadText); err == nil {
				t.Fatal("started text before media stopped")
			}
			for _, call := range runner.calls {
				if strings.Contains(call, "--user start") {
					t.Fatalf("unsafe start: %s", call)
				}
			}
		})
	}
}

func TestStopServiceStartsOnlyAfterBothUnitsAndCgroupsAreReleased(t *testing.T) {
	for _, workload := range []control.Workload{control.WorkloadText, control.WorkloadMedia} {
		t.Run(string(workload), func(t *testing.T) {
			runner := &fakeRunner{outputs: map[string][]byte{textShowCommand: stoppedOutput(), mediaShowCommand: stoppedOutput(), gpuMemoryCommand: []byte("0\n")}}
			manager := strictManager(t, runner)
			if err := manager.Start(context.Background(), workload); err != nil {
				t.Fatal(err)
			}
			if got := runner.calls[len(runner.calls)-1]; got != "/usr/bin/true --user start -- "+string(workload)+".service" {
				t.Fatal(got)
			}
			opposing := "media.service"
			if workload == control.WorkloadMedia {
				opposing = "text.service"
			}
			writeEvents(t, manager.cgroups.root, "workloads/"+opposing, "populated 1\n")
			runner.calls = nil
			if err := manager.Start(context.Background(), workload); err == nil {
				t.Fatal("ignored populated workload cgroup")
			}
			for _, call := range runner.calls {
				if strings.Contains(call, "--user start") {
					t.Fatal(call)
				}
			}
		})
	}
}

func TestObserveReportsConcurrentRuntimesForSupervisorArbitration(t *testing.T) {
	active := []byte("LoadState=loaded\nActiveState=active\nSubState=running\n")
	runner := &fakeRunner{outputs: map[string][]byte{textShowCommand: active, mediaShowCommand: active}}
	manager := strictManager(t, runner)
	snapshot, err := manager.Observe(context.Background())
	if err != nil || !snapshot.Workloads[control.WorkloadText].Active || !snapshot.Workloads[control.WorkloadMedia].Active {
		t.Fatalf("concurrent runtimes not reported: %#v, %v", snapshot, err)
	}
	runner.errs = map[string]error{textShowCommand: errors.New("probe failed")}
	if err := manager.Start(context.Background(), control.WorkloadMedia); err == nil {
		t.Fatal("ignored probe failure")
	}
	runner.errs = map[string]error{"/usr/bin/true --user stop -- media.service": errors.New("stop failed")}
	if err := manager.Stop(context.Background(), control.WorkloadMedia); err == nil {
		t.Fatal("ignored stop failure")
	}
}
