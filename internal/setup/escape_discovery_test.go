package setup

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

// Discovery reports the escape routes as an advisory list. The scan uses
// the backend command runner and the systemctl of the profile. Without the
// scan, the list is null. A scan without findings gives an empty list.
func TestDiscoveryReportsEscapeRoutes(t *testing.T) {
	backend, home, _ := fixture(t)
	var commands []string
	backend.runCommand = func(_ context.Context, name string, args ...string) ([]byte, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	var systemctl string
	found := []gpuruntime.EscapeRoute{{Route: "cron", Item: "/etc/cron.deny does not deny alice"}}
	backend.escapeRoutes = func(ctx context.Context, run gpuruntime.CommandFunc, path string) []gpuruntime.EscapeRoute {
		systemctl = path
		_, _ = run(ctx, path, "show")
		return found
	}
	got, err := backend.Discover(context.Background(), home)
	if err != nil || len(got.EscapeRoutes) != 1 || systemctl != "/usr/bin/systemctl" || commands[len(commands)-1] != "/usr/bin/systemctl show" {
		t.Fatalf("discovery = %+v, %v with %s: %v", got.EscapeRoutes, err, systemctl, commands)
	}
	data, err := json.Marshal(got)
	if err != nil || !strings.Contains(string(data), `"escapeRoutes":[{"route":"cron","item":"/etc/cron.deny does not deny alice"}]`) {
		t.Fatalf("discovery JSON = %s, %v", data, err)
	}
	found = []gpuruntime.EscapeRoute{}
	got, err = backend.Discover(context.Background(), home)
	if data, _ := json.Marshal(got); err != nil || !strings.Contains(string(data), `"escapeRoutes":[]`) {
		t.Fatalf("discovery without findings = %s, %v", data, err)
	}
	backend.escapeRoutes = nil
	got, err = backend.Discover(context.Background(), home)
	if data, _ := json.Marshal(got); err != nil || !strings.Contains(string(data), `"escapeRoutes":null`) {
		t.Fatalf("discovery without the scan = %s, %v", data, err)
	}
	if SystemBackend().escapeRoutes == nil {
		t.Fatal("the system backend does not scan the escape routes")
	}
}

// The scan uses the systemctl path of the profile and the backend runner.
// Without a profile path, it uses the fixed setup path.
func TestEscapeRouteScanUsesProfileSystemctl(t *testing.T) {
	var commands []string
	backend := Backend{
		runCommand: func(_ context.Context, name string, _ ...string) ([]byte, error) {
			commands = append(commands, name)
			return nil, nil
		},
		escapeRoutes: func(ctx context.Context, run gpuruntime.CommandFunc, path string) []gpuruntime.EscapeRoute {
			_, _ = run(ctx, path)
			return nil
		},
	}
	backend.discoverEscapeRoutes(context.Background(), &Discovery{Request: Request{Profile: Profile{SystemctlPath: "/opt/bin/systemctl"}}})
	backend.discoverEscapeRoutes(context.Background(), &Discovery{})
	if len(commands) != 2 || commands[0] != "/opt/bin/systemctl" || commands[1] != automaticSystemctlPath {
		t.Fatalf("systemctl commands = %q", commands)
	}
}
