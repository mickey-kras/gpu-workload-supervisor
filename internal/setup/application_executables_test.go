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
		b := Backend{discoverExecutables: func(context.Context, string) []ApplicationCandidate {
			request := ProbeRequest{App: "ollama", Reference: "/usr/bin/ollama", ReferenceKind: "application"}
			found := probeSelectedExecutable(request, candidate(request), func(string, string) error { return nil })
			return []ApplicationCandidate{found}
		}}
		b.discoverInstalledExecutables(context.Background(), &result, t.TempDir())
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

func TestFolderFallbackFindsBlockedInstallationWithoutExecutionOrTraversal(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "bin", "vllm")
	if err := os.WriteFile(path, []byte("not executed"), 0700); err != nil {
		t.Fatal(err)
	}
	request := ProbeRequest{App: "vllm", Reference: root, ReferenceKind: "application-directory"}
	inspected := []string{}
	result, err := probeReferenceWithExecutableValidator(context.Background(), request, candidate(request), func(app, selected string) error {
		inspected = append(inspected, selected)
		return errors.New("executable is not root-owned")
	})
	if err != nil || len(inspected) != 1 || inspected[0] != path || !result.Installed || result.Recognized || result.Binding != nil {
		t.Fatalf("folder fallback bypassed qualification: %+v %v %v", result, err, inspected)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "other"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "other", "vllm"), nil, 0700); err != nil {
		t.Fatal(err)
	}
	result = probeExecutableDirectory(context.Background(), request, func(string, string) error { t.Fatal("nested file was inspected"); return nil })
	if result.Recognized || result.Installed || result.InstanceStatus == "missing" {
		t.Fatalf("bounded fallback claims absent/qualified: %+v", result)
	}
}

func TestRelevantRootsAndSavedSelectionsRemainBounded(t *testing.T) {
	home := t.TempDir()
	result := relevantExecutablePaths(home)
	if len(result) != 9 {
		t.Fatalf("unexpected fixed candidates %d", len(result))
	}
	for _, request := range result {
		if err := request.validate(); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(request.Reference, "/AI/") {
			t.Fatal("guessed arbitrary home directory")
		}
	}
}

func TestPhysicalGPUInventoryRejectsAmbiguousOrNonPhysicalEvidence(t *testing.T) {
	uuid := "GPU-01234567-89ab-cdef-0123-456789abcdef"
	if result := physicalGPUInventory([]byte(uuid + "\n")); len(result) != 1 || result[0] != uuid {
		t.Fatal(result)
	}
	for _, value := range []string{"0", "GPU-0123", "MIG-01234567-89ab-cdef-0123-456789abcdef", uuid + "\n" + uuid, strings.Repeat("x", 4097)} {
		if len(physicalGPUInventory([]byte(value))) != 0 {
			t.Fatalf("accepted ambiguous inventory %q", value)
		}
	}
}

func TestExecutableDiscoveryAccessErrorDoesNotProveInstallation(t *testing.T) {
	request := ProbeRequest{App: "vllm", Reference: "/unreadable/vllm", ReferenceKind: "application"}
	found := inspectInstalledExecutables(context.Background(), []ProbeRequest{request}, func(string) (os.FileInfo, error) { return nil, os.ErrPermission }, func(string, string) error { return os.ErrPermission }, nil)
	if len(found) != 1 || found[0].Installed || found[0].Recognized || found[0].Binding != nil {
		t.Fatalf("access failure claimed installation: %+v", found)
	}
}

func TestGPUInventoryTrustAndQueryFailuresAreSanitized(t *testing.T) {
	uuid := "GPU-01234567-89ab-cdef-0123-456789abcdef"
	for _, tt := range []struct {
		trust  error
		query  error
		data   string
		status string
	}{
		{os.ErrPermission, nil, uuid, "untrusted"}, {nil, errors.New("private-query-secret"), "private-output-secret", "unavailable"}, {nil, nil, "private-output-secret", "invalid"}, {nil, nil, uuid, "available"},
	} {
		calls := 0
		uuids, status := inspectGPUInventory(context.Background(), func(_ context.Context, path string, args ...string) ([]byte, error) {
			calls++
			if path != gpuInventoryExecutable || strings.Join(args, " ") != "--query-gpu=uuid --format=csv,noheader" {
				t.Fatal(path, args)
			}
			return []byte(tt.data), tt.query
		}, func(path string) error {
			if path != gpuInventoryExecutable {
				t.Fatal(path)
			}
			return tt.trust
		})
		if status != tt.status || (tt.trust != nil && calls != 0) || (status != "available" && len(uuids) != 0) {
			t.Fatal(uuids, status, calls)
		}
	}
}

func TestSavedExecutableSelectionIsRediscoveredWithoutHomeScan(t *testing.T) {
	home := t.TempDir()
	draft := Draft{ID: "selected", Label: "Selected", App: "vllm", Reference: "/arbitrary/installation/vllm", ReferenceKind: "application"}
	if _, err := SaveDrafts(home, DraftRequest{Version: 1, Drafts: []Draft{draft}}); err != nil {
		t.Fatal(err)
	}
	paths := relevantExecutablePaths(home)
	count := 0
	for _, path := range paths {
		if path.Reference == draft.Reference {
			count++
		}
	}
	if count != 1 || len(paths) != 10 {
		t.Fatalf("saved explicit reference omitted/unbounded: %+v", paths)
	}
}

func TestStoppedServiceWithMissingExecutableDoesNotClaimInstalled(t *testing.T) {
	b, draft, _ := automaticFixture(t, "ollama")
	b.inspectAutomatic = func(string, string) (gpuruntime.AutomaticLaunch, error) {
		return gpuruntime.AutomaticLaunch{}, errors.New("executable is missing")
	}
	result := Discovery{}
	b.discoverUnit(context.Background(), &result, draft.Binding.Unit)
	if len(result.Applications) != 1 {
		t.Fatal(result.Applications)
	}
	found := result.Applications[0]
	if found.Installed || found.Recognized || found.Binding != nil || found.ConfigurationStatus != inspectionFailedStatus {
		t.Fatalf("service metadata falsely claimed installed: %+v", found)
	}
}

func TestExecutableDiscoveryUsesExplicitAccountHomeDespiteAmbientHOME(t *testing.T) {
	home := t.TempDir()
	alternate := t.TempDir()
	t.Setenv("HOME", alternate)
	for _, root := range []string{home, alternate} {
		if err := os.MkdirAll(filepath.Join(root, ".local/bin"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".local/bin/vllm"), nil, 0700); err != nil {
			t.Fatal(err)
		}
	}
	selected := filepath.Join(home, "custom/ollama")
	if err := os.MkdirAll(filepath.Dir(selected), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(selected, nil, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveDrafts(home, DraftRequest{Version: 1, Drafts: []Draft{{ID: "custom", Label: "Custom", App: "ollama", Reference: selected, ReferenceKind: "application"}}}); err != nil {
		t.Fatal(err)
	}
	found := installedExecutables(context.Background(), home)
	expected := map[string]bool{filepath.Join(home, ".local/bin/vllm"): false, selected: false}
	for _, candidate := range found {
		if strings.HasPrefix(candidate.Reference, alternate+string(os.PathSeparator)) {
			t.Fatal("ambient HOME affected discovery", candidate.Reference)
		}
		if _, ok := expected[candidate.Reference]; ok {
			expected[candidate.Reference] = true
		}
	}
	for path, present := range expected {
		if !present {
			t.Fatal("explicit account installation omitted", path)
		}
	}
	t.Setenv("HOME", "")
	if got := installedExecutables(context.Background(), home); len(got) != len(found) {
		t.Fatalf("unset HOME changed explicit account discovery: %d != %d", len(got), len(found))
	}
}

func TestWrongApplicationSelectionCannotHideCorrectExecutableAtSamePath(t *testing.T) {
	path := "/custom/bin/llama-server"
	requests := []ProbeRequest{
		{App: "vllm", Reference: path, ReferenceKind: "application"},
		{App: appLlamaCPP, Reference: path, ReferenceKind: "application"},
		{App: appLlamaCPP, Reference: path, ReferenceKind: "application"},
	}
	calls := 0
	found := inspectInstalledExecutables(context.Background(), requests, func(string) (os.FileInfo, error) { return nil, nil }, func(app, reference string) error {
		calls++
		if reference != path {
			t.Fatal(reference)
		}
		if app != appLlamaCPP {
			return errors.New("selected application does not match executable")
		}
		return nil
	}, func(reference string) (string, error) { return reference, nil })
	if calls != 2 || len(found) != 2 {
		t.Fatalf("different app hidden or same app repeated: calls=%d candidates=%+v", calls, found)
	}
	if found[0].Recognized || found[0].Binding != nil || !found[1].Recognized || found[1].App != appLlamaCPP || found[1].Binding == nil {
		t.Fatalf("invalid app request suppressed qualification: %+v", found)
	}
}
