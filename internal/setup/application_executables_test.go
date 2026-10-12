package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

func TestExecutableDiscoveryPathsAreBoundedExplicitCandidates(t *testing.T) {
	path := strings.Join([]string{"", ".", "relative/bin", "/env/bin/../bin", "/env/bin", "/env/bin", "/usr/bin"}, string(os.PathListSeparator))
	requests := executableDiscoveryPaths(path)
	if len(requests) != 12 {
		t.Fatalf("unexpected bounded locations: %+v", requests)
	}
	for _, request := range requests {
		if err := request.validate(); err != nil {
			t.Fatalf("unsafe discovery request %+v: %v", request, err)
		}
		if request.ReferenceKind != "application" || request.Endpoint != "" {
			t.Fatalf("not explicit executable evidence: %+v", request)
		}
	}
	if got := len(executableDiscoveryPaths(strings.Repeat("/env/bin:", 100000))); got != 12 {
		t.Fatalf("duplicate or unbounded PATH candidates: %d", got)
	}
	entries := make([]string, 100)
	for i := range entries {
		entries[i] = filepath.Join("/environments", strings.Repeat("a", i+1))
	}
	if got := len(executableDiscoveryPaths(strings.Join(entries, ":"))); got != (3+32)*3 {
		t.Fatalf("PATH discovery exceeds 32 directories: %d", got)
	}
}

func TestInstalledExecutableInspectionSharesQualificationAndDeduplicatesTrustedAliases(t *testing.T) {
	requests := []ProbeRequest{
		{App: "ollama", Reference: "/usr/bin/ollama", ReferenceKind: "application"},
		{App: "ollama", Reference: "/bin/ollama", ReferenceKind: "application"},
		{App: "vllm", Reference: "/env/bin/vllm", ReferenceKind: "application"},
		{App: appLlamaCPP, Reference: "/usr/bin/llama-server", ReferenceKind: "application"},
	}
	var qualified []string
	found := inspectInstalledExecutables(context.Background(), requests, func(path string) (os.FileInfo, error) {
		if strings.HasSuffix(path, "llama-server") {
			return nil, os.ErrNotExist
		}
		return nil, nil
	}, func(app, path string) error {
		qualified = append(qualified, path)
		if app == "vllm" {
			return errors.New("untrusted selected executable")
		}
		return nil
	}, func(string) (string, error) { return "/usr/bin/ollama", nil })
	if len(qualified) != 3 || len(found) != 2 {
		t.Fatalf("missing candidates inspected or trusted alias duplicated: %v %+v", qualified, found)
	}
	if !found[0].Recognized || found[0].SourceKind != "owned" || found[0].Binding.Owned.Executable != "/usr/bin/ollama" {
		t.Fatalf("supported executable not ready for managed flow: %+v", found[0])
	}
	if found[1].Recognized || found[1].Binding != nil || found[1].ConfigurationStatus != inspectionFailedStatus {
		t.Fatalf("untrusted executable was usable or hidden: %+v", found[1])
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := inspectInstalledExecutables(ctx, requests, func(string) (os.FileInfo, error) {
		t.Fatal("cancelled discovery inspected the filesystem")
		return nil, nil
	}, nil, nil); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestExecutableDiscoveryKeepsEndpointObservationSeparateAndPrefersRecognizedService(t *testing.T) {
	for _, hasService := range []bool{false, true} {
		result := Discovery{Applications: []ApplicationCandidate{{App: "ollama", SourceKind: "endpoint", InstanceStatus: "unreachable"}}}
		if hasService {
			result.Applications = append(result.Applications, ApplicationCandidate{App: "ollama", Unit: "existing.service", Recognized: true})
		}
		b := Backend{discoverExecutables: func(context.Context) []ApplicationCandidate {
			request := ProbeRequest{App: "ollama", Reference: "/usr/bin/ollama", ReferenceKind: "application"}
			found, _ := probeSelectedExecutable(request, candidate(request), func(string, string) error { return nil })
			return []ApplicationCandidate{found}
		}}
		b.discoverInstalledExecutables(context.Background(), &result)
		if len(result.Applications) != 2 || result.Applications[0].Recognized || result.Applications[0].Binding != nil || !result.Applications[1].Recognized {
			t.Fatalf("endpoint/installation identities conflated: %+v", result.Applications)
		}
		if hasService && result.Applications[1].Unit == "" {
			t.Fatal("recognized service replaced by separate owned launch")
		}
	}
}

func TestUnresolvedGPUIdentityIsInspectableWithoutInstallationFailure(t *testing.T) {
	b, draft, _ := automaticFixture(t, "ollama")
	b.inspectAutomatic = func(string, string) (gpuruntime.AutomaticLaunch, error) {
		return gpuruntime.AutomaticLaunch{}, gpuruntime.ErrGPUIdentityUnresolved
	}
	result := Discovery{}
	b.discoverUnit(context.Background(), &result, draft.Binding.Unit)
	if len(result.Applications) != 1 || result.Applications[0].ConfigurationStatus != "gpu-unresolved" || result.Applications[0].Recognized || result.Applications[0].Binding != nil {
		t.Fatalf("unresolved GPU became usable or disappeared: %+v", result.Applications)
	}
}
