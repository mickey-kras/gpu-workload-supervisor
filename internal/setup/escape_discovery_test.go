package setup

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

// Discovery reports the escape routes as an advisory list. Without the
// scan, the list is absent.
func TestDiscoveryReportsEscapeRoutes(t *testing.T) {
	backend, home, _ := fixture(t)
	backend.runCommand = func(context.Context, string, ...string) ([]byte, error) { return nil, nil }
	calls := 0
	backend.escapeRoutes = func(context.Context) []gpuruntime.EscapeRoute {
		calls++
		return []gpuruntime.EscapeRoute{{Route: "cron", Item: "/etc/cron.deny does not deny alice"}}
	}
	got, err := backend.Discover(context.Background(), home)
	if err != nil || calls != 1 || len(got.EscapeRoutes) != 1 {
		t.Fatalf("discovery = %+v, %v after %d scans", got.EscapeRoutes, err, calls)
	}
	data, err := json.Marshal(got)
	if err != nil || !strings.Contains(string(data), `"escapeRoutes":[{"route":"cron","item":"/etc/cron.deny does not deny alice"}]`) {
		t.Fatalf("discovery JSON = %s, %v", data, err)
	}
	backend.escapeRoutes = nil
	got, err = backend.Discover(context.Background(), home)
	if data, _ := json.Marshal(got); err != nil || strings.Contains(string(data), "escapeRoutes") {
		t.Fatalf("discovery without the scan = %s, %v", data, err)
	}
	if SystemBackend().escapeRoutes == nil {
		t.Fatal("the system backend does not scan the escape routes")
	}
}
