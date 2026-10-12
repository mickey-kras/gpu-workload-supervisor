package setup

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

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
				name = "llama-server"
			}
			requests = append(requests, ProbeRequest{App: app, Reference: filepath.Join(directory, name), ReferenceKind: "application"})
		}
	}
	return requests
}

func installedExecutables(ctx context.Context) []ApplicationCandidate {
	return inspectInstalledExecutables(ctx, executableDiscoveryPaths(os.Getenv("PATH")), os.Lstat, gpuruntime.ValidateSelectedNativeExecutable, filepath.EvalSymlinks)
}

func inspectInstalledExecutables(ctx context.Context, requests []ProbeRequest, stat func(string) (os.FileInfo, error), validate func(string, string) error, resolve func(string) (string, error)) []ApplicationCandidate {
	seen := map[string]bool{}
	var found []ApplicationCandidate
	for _, request := range requests {
		if ctx.Err() != nil {
			break
		}
		if _, err := stat(request.Reference); os.IsNotExist(err) {
			continue
		}
		result, _ := probeSelectedExecutable(request, candidate(request), validate)
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
