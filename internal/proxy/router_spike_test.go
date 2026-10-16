package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestRouterSpikeRejectsLifecycleAndMetricsRoutes(t *testing.T) {
	for _, route := range []Route{
		{Method: "POST", Path: "/models"},
		{Method: "DELETE", Path: "/models"},
		{Method: "POST", Path: "/models/load"},
		{Method: "POST", Path: "/models/unload"},
		{Method: "GET", Path: "/models"},
		{Method: "GET", Path: "/models/sse"},
		{Method: "GET", Path: "/metrics"},
	} {
		t.Run(route.Method+route.Path, func(t *testing.T) {
			s, config, _, calls := nativeProxyFixture(t)
			s.catalog.Catalog.Profiles[0].NativeModel.Runtime = "llama.cpp"
			config.ExecutionRoutes = []Route{{Method: "POST", Path: "/v1/chat/completions"}}
			h, err := NewWithContext(context.Background(), s, config)
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(route.Method, route.Path, strings.NewReader(`{"model":"selected"}`)))
			if w.Code != http.StatusNotFound || *calls != 0 || len(s.admitted) != 0 {
				t.Fatalf("unclassified router route escaped: status=%d calls=%d admitted=%d", w.Code, *calls, len(s.admitted))
			}
			for _, classification := range []string{"execution", "read", "passthrough"} {
				candidate := config
				switch classification {
				case "execution":
					candidate.ExecutionRoutes = []Route{route}
				case "read":
					candidate.ReadOnlyRoutes = []Route{route}
				case "passthrough":
					candidate.PassthroughRoutes = []Route{route}
				}
				if _, err := NewWithContext(context.Background(), s, candidate); err == nil {
					t.Fatalf("router route accepted as %s", classification)
				}
			}
		})
	}
}

func TestRouterSpikeRejectsPerRequestAutoload(t *testing.T) {
	for _, owner := range []control.Owner{control.OwnerUser, control.OwnerSupervisor} {
		t.Run(string(owner), func(t *testing.T) {
			s, config, _, calls := nativeProxyFixture(t)
			s.state.Owner = owner
			s.catalog.Catalog.Profiles[0].NativeModel.Runtime = "llama.cpp"
			config.ExecutionRoutes = []Route{{Method: "POST", Path: "/v1/chat/completions"}}
			h, err := NewWithContext(context.Background(), s, config)
			if err != nil {
				t.Fatal(err)
			}
			for _, query := range []string{"autoload=true", "autoload=1", "%61utoload=true", "autoload=false&autoload=true", "autoload=false"} {
				r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?"+query, strings.NewReader(`{"model":"selected","messages":[]}`))
				addLeaseHeaders(r, s.state.LeaseFence)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != http.StatusBadRequest || *calls != 0 || len(s.admitted) != 0 {
					t.Fatalf("autoload override escaped: query=%s status=%d calls=%d admitted=%d", query, w.Code, *calls, len(s.admitted))
				}
			}
		})
	}
}
