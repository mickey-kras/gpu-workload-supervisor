package runtime

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

// This is an unqualified configuration fixture, never an installable unit.
func TestRouterSpikeCannotQualifyAsDirectLaunch(t *testing.T) {
	raw, err := os.ReadFile("testdata/llama.cpp-v0.6.0/router.service")
	if err != nil {
		t.Fatal(err)
	}
	// Trust executable paths to isolate the unsupported launch grammar.
	validate := func(string) error { return nil }
	if _, err := inspectAutomaticLaunch(raw, "llama.cpp", validate); err == nil {
		t.Fatal("unqualified router accepted by automatic onboarding")
	}
	n := control.NativeModel{Runtime: "llama.cpp", Endpoint: "http://127.0.0.1:9000", Model: "selected"}
	if err := qualifyNativeLaunchWithValidator(raw, n, validate); err == nil {
		t.Fatal("unqualified router accepted as direct launch")
	}
}

func TestRouterSpikeReadinessThroughNativeProbe(t *testing.T) {
	loaded, err := os.ReadFile("testdata/llama.cpp-v0.6.0/models-loaded.json")
	if err != nil {
		t.Fatal(err)
	}
	failed, err := os.ReadFile("testdata/llama.cpp-v0.6.0/models-failed.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		body  string
		ready bool
	}{
		{"selected-loaded", string(loaded), true},
		{"child-exited", string(failed), false},
		{"selected-unloaded", `{"data":[{"id":"selected","status":{"value":"unloaded"}}]}`, false},
		{"selected-loading", `{"data":[{"id":"selected","status":{"value":"loading"}}]}`, false},
		{"wrong-model-loaded", strings.Replace(string(loaded), `"id": "selected"`, `"id": "other"`, 1), false},
		{"loaded-but-failed", `{"data":[{"id":"selected","status":{"value":"loaded","failed":true}}]}`, false},
		{"loaded-but-exited", `{"data":[{"id":"selected","status":{"value":"loaded","exit_code":1}}]}`, false},
		// The direct probe requires a singleton list; router catalog entries
		// are not filtered to loaded children. This remains a migration gate.
		{"unloaded-catalog-entry", `{"data":[{"id":"selected","status":{"value":"loaded"}},{"id":"other","status":{"value":"unloaded"}}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				paths = append(paths, r.URL.RequestURI())
				mu.Unlock()
				if r.Method != http.MethodGet || r.URL.RawQuery != "" {
					t.Error("readiness attempted a mutation or query override")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				switch r.URL.Path {
				case "/health":
					fmt.Fprint(w, `{"status":"ok"}`)
				case "/v1/models":
					fmt.Fprint(w, tc.body)
				default:
					t.Error("readiness requested management or model-specific endpoint")
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			m := &SystemdManager{client: server.Client()}
			err := m.nativeReady(context.Background(), control.NativeModel{Runtime: "llama.cpp", Model: "selected", Endpoint: server.URL})
			if (err == nil) != tc.ready {
				t.Fatalf("ready = %v, err = %v", tc.ready, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if strings.Join(paths, ",") != "/health,/v1/models" {
				t.Fatalf("probe paths = %v", paths)
			}
		})
	}
}
