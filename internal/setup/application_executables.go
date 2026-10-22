package setup

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

const llamaServerExecutable = "llama-server"

// Only known executable names at standard locations and a bounded number of
// explicit PATH directories are inspected. No directories are scanned and no
// executable is run. Qualification is shared with explicit executable selection.
func executableDiscoveryPaths(path string) []ProbeRequest {
	directories := []string{"/usr/local/bin", "/usr/bin", "/bin"}
	entries := strings.SplitN(path, string(os.PathListSeparator), 33)
	if len(entries) > 32 {
		entries = entries[:32]
	}
	directories = append(directories, entries...)
	seen := map[string]bool{}
	var requests []ProbeRequest
	for _, directory := range directories {
		if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || len(directory) > 4096 || seen[directory] {
			continue
		}
		seen[directory] = true
		for _, app := range []string{"ollama", appLlamaCPP, "vllm"} {
			name := app
			if app == appLlamaCPP {
				name = llamaServerExecutable
			}
			requests = append(requests, ProbeRequest{App: app, Reference: filepath.Join(directory, name), ReferenceKind: "application"})
		}
	}
	return requests
}

func installedExecutables(ctx context.Context, home string) []ApplicationCandidate {
	requests := executableDiscoveryPaths(os.Getenv("PATH"))
	requests = append(requests, relevantExecutablePaths(home)...)
	return inspectInstalledExecutables(ctx, requests, os.Lstat, gpuruntime.ValidateSelectedNativeExecutable, filepath.EvalSymlinks)
}

func inspectInstalledExecutables(ctx context.Context, requests []ProbeRequest, stat func(string) (os.FileInfo, error), validate func(string, string) error, resolve func(string) (string, error)) []ApplicationCandidate {
	seen := map[string]bool{}
	type installationSelection struct{ app, reference string }
	inspected := map[installationSelection]bool{}
	var found []ApplicationCandidate
	for _, request := range requests {
		if ctx.Err() != nil {
			break
		}
		selection := installationSelection{app: request.App, reference: request.Reference}
		if inspected[selection] {
			continue
		}
		inspected[selection] = true
		_, statErr := stat(request.Reference)
		if os.IsNotExist(statErr) {
			continue
		}
		result := probeSelectedExecutable(request, candidate(request), validate)
		result.Installed = statErr == nil
		if result.Recognized {
			// Trusted aliases, including /bin -> /usr/bin, are one installation.
			key, err := resolve(request.Reference)
			if err != nil {
				continue
			}
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		found = append(found, result)
	}
	return found
}

// Relevant roots have fixed names. An arbitrary installation uses explicit selection.
func relevantExecutablePaths(home string) []ProbeRequest {
	paths := []ProbeRequest{}
	if filepath.IsAbs(home) && filepath.Clean(home) == home {
		for _, app := range []string{"ollama", appLlamaCPP, "vllm"} {
			name := app
			if app == appLlamaCPP {
				name = llamaServerExecutable
			}
			paths = append(paths, ProbeRequest{App: app, Reference: filepath.Join(home, ".local/bin", name), ReferenceKind: "application"})
		}
		paths = append(paths, savedExecutablePaths(home)...)
		paths = append(paths, localAIExecutablePaths(home)...)
	}
	for _, app := range []string{"ollama", appLlamaCPP, "vllm"} {
		name := app
		if app == appLlamaCPP {
			name = llamaServerExecutable
		}
		for _, dir := range []string{filepath.Join("/opt", app), filepath.Join("/opt", app, "bin")} {
			paths = append(paths, ProbeRequest{App: app, Reference: filepath.Join(dir, name), ReferenceKind: "application"})
		}
	}
	return paths
}

func localAIExecutablePaths(home string) []ProbeRequest {
	paths := []ProbeRequest{}
	for _, app := range []string{"ollama", "vllm"} {
		for _, directory := range []string{app, app + "-supervisor"} {
			for _, bin := range []string{"bin", ".venv/bin"} {
				paths = append(paths, ProbeRequest{App: app, Reference: filepath.Join(home, "AI", directory, bin, app), ReferenceKind: "application"})
			}
		}
	}
	// Inspect one bounded level of the versioned Ollama install root. Never
	// walk model/cache trees or run a discovered executable.
	root := filepath.Join(home, "AI", "ollama")
	directory, err := os.OpenFile(root, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return paths
	}
	defer directory.Close()
	entries, err := directory.ReadDir(33)
	if err != nil && len(entries) == 0 || len(entries) > 32 {
		return paths
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.IsDir() && entry.Name()[0] >= '0' && entry.Name()[0] <= '9' {
			paths = append(paths, ProbeRequest{App: "ollama", Reference: filepath.Join(root, entry.Name(), "bin", "ollama"), ReferenceKind: "application"})
		}
	}
	return paths
}

func savedExecutablePaths(home string) []ProbeRequest {
	saved, err := ReadDrafts(home)
	if err != nil {
		return nil
	}
	var paths []ProbeRequest
	for _, draft := range saved.Drafts {
		path := ""
		if draft.ReferenceKind == "application" && draft.App != "comfyui" {
			path = draft.Reference
		}
		if draft.Binding != nil && draft.Binding.Owned != nil {
			path = draft.Binding.Owned.Executable
		}
		if path != "" {
			paths = append(paths, ProbeRequest{App: draft.App, Reference: path, ReferenceKind: "application"})
		}
	}
	return paths
}
