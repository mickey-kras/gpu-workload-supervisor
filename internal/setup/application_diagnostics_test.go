package setup

import (
	"context"
	"errors"
	"testing"

	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

func TestDiscoveryCandidateRetainsAllLaunchDiagnosticsWithoutBinding(t *testing.T) {
	b, draft, _ := automaticFixture(t, "vllm")
	b.inspectAutomatic = func(string, string) (gpuruntime.AutomaticLaunch, error) {
		return gpuruntime.AutomaticLaunch{}, gpuruntime.CombineLaunchErrors(gpuruntime.ErrGPUIdentityUnresolved, errors.New("Environment setting is not qualified"))
	}
	var result Discovery
	b.discoverUnit(context.Background(), &result, draft.Binding.Unit)
	if len(result.Applications) != 1 {
		t.Fatal(result.Applications)
	}
	found := result.Applications[0]
	if found.Recognized || found.Binding != nil || found.ConfigurationStatus != "gpu-unresolved" || len(found.Diagnostics) != 2 {
		t.Fatalf("unsafe or incomplete candidate: %+v", found)
	}
}

func TestSelectedExecutableCandidateRetainsWrappedDiagnostics(t *testing.T) {
	request := ProbeRequest{App: "vllm", Reference: "/usr/bin/vllm", ReferenceKind: "application"}
	got := probeSelectedExecutable(request, candidate(request), func(string, string) error {
		return gpuruntime.CombineLaunchErrors(errors.New("executable ownership is not trusted"), errors.New("executable ancestor is writable"))
	})
	if got.Recognized || got.Binding != nil || len(got.Diagnostics) != 2 || got.ConfigurationStatus != inspectionFailedStatus {
		t.Fatal(got)
	}
}

func TestPrepareRequiresLoadedAccountHomeWorkingDirectory(t *testing.T) {
	backend, draft, metadata := automaticFixture(t, "vllm")
	inspect := backend.inspectAutomatic
	backend.inspectAutomatic = func(path, app string) (gpuruntime.AutomaticLaunch, error) {
		launch, err := inspect(path, app)
		launch.HomeWorkingDirectory = "/opt/qualified-fixture"
		return launch, err
	}
	metadata[draft.Binding.Unit]["WorkingDirectory"] = "/opt/qualified-fixture"
	if _, err := backend.Prepare(context.Background(), PrepareRequest{Draft: draft}); err != nil {
		t.Fatal(err)
	}
	metadata[draft.Binding.Unit]["WorkingDirectory"] = "/private-different-directory"
	prepared, err := backend.Prepare(context.Background(), PrepareRequest{Draft: draft})
	if !errors.Is(err, gpuruntime.ErrLaunchChanged) || prepared.Profile.NativeModel != nil {
		t.Fatalf("loaded directory drift permitted binding: %+v %v", prepared, err)
	}
}
