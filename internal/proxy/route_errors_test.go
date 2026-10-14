package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestRouteRejectionDistinguishesUnknownPathsAndWrongMethods(t *testing.T) {
	target, _ := url.Parse("http://127.0.0.1:1")
	stateStore := &fakeStore{state: admittedState(control.OwnerSupervisor)}
	config := adapterConfig(target)
	config.CompletionPath = "/finish"
	config.ReadOnlyRoutes = append(config.ReadOnlyRoutes, Route{Method: "OPTIONS", Path: "/execute"})
	config.PassthroughRoutes = append(config.PassthroughRoutes, Route{Method: "PUT", Path: "/execute"}, Route{Method: "GET", Path: "/finish"})
	handler, err := NewWithContext(context.Background(), stateStore, config)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, method, path, credential, allow, code string
		status                                      int
	}{
		{"unknown read", "GET", "/unknown", executionCredential, "", "route_not_allowed", 404},
		{"unknown mutation", "POST", "/unknown", executionCredential, "", "route_not_allowed", 404},
		{"exact path only", "POST", "/execute/more", executionCredential, "", "route_not_allowed", 404},
		{"multiple route classes", "DELETE", "/execute", executionCredential, "OPTIONS, POST, PUT", "route_not_allowed", 405},
		{"no implicit HEAD", "HEAD", "/monitor", executionCredential, "GET", "route_not_allowed", 405},
		{"passthrough", "POST", "/edit", executionCredential, "PATCH", "route_not_allowed", 405},
		{"completion plus configured method", "DELETE", "/finish", executionCredential, "GET, POST", "route_not_allowed", 405},
		{"old completion path unknown", "GET", DefaultCompletionPath, executionCredential, "", "route_not_allowed", 404},
		{"canonical validation first", "DELETE", "/execute/", executionCredential, "", "path_not_canonical", 400},
		{"unknown unauthenticated", "GET", "/unknown", "", "", "ingress_unauthorized", 401},
		{"wrong method unauthenticated", "DELETE", "/execute", "", "", "ingress_unauthorized", 401},
		{"completion credential cannot probe execution routes", "DELETE", "/finish", completionCredential, "", "ingress_unauthorized", 401},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, nil)
			request.Header.Set(ExecutionCredentialHeader, test.credential)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			assertProxyError(t, response, test.status, test.code)
			if got := response.Header().Get("Allow"); got != test.allow {
				t.Fatalf("Allow = %q, want %q", got, test.allow)
			}
		})
	}
	if len(stateStore.admitted) != 0 || len(stateStore.finished) != 0 {
		t.Fatalf("rejection changed work: admitted=%v finished=%v", stateStore.admitted, stateStore.finished)
	}
}

func assertProxyError(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if response.Code != status || response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("response = %d %v %q", response.Code, response.Header(), response.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 1 || body["error"] != code {
		t.Fatalf("error body = %v, want %q", body, code)
	}
}

func TestDefaultCompletionPathRejectsWrongMethodWithAllow(t *testing.T) {
	handler := testHandler(t, &fakeStore{state: admittedState(control.OwnerSupervisor)}, "http://127.0.0.1:1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", DefaultCompletionPath, nil))
	assertProxyError(t, response, 405, "route_not_allowed")
	if got := response.Header().Get("Allow"); got != "POST" {
		t.Fatalf("Allow = %q, want POST", got)
	}
}

func TestRouteConfigurationRejectsInvalidMethodTokens(t *testing.T) {
	target, _ := url.Parse("http://127.0.0.1:1")
	for _, method := range []string{"FOO BAR", "GET,POST", "GET\tPOST", "GET\r\nAllow: POST", "GET/POST", "GÉT", "GET\x00POST"} {
		for _, classification := range []string{"execution", "read-only", "passthrough"} {
			t.Run(classification+"/"+method, func(t *testing.T) {
				config := Config{Upstream: target, Workload: control.WorkloadMedia, ExecutionRoutes: []Route{{Method: "POST", Path: "/execute"}}}
				route := Route{Method: method, Path: "/configured"}
				switch classification {
				case "execution":
					config.ExecutionRoutes = []Route{route}
				case "read-only":
					config.ReadOnlyRoutes = []Route{route}
				case "passthrough":
					config.PassthroughRoutes = []Route{route}
				}
				if _, err := NewWithContext(context.Background(), &fakeStore{}, config); err == nil {
					t.Fatal("invalid method token accepted")
				}
			})
		}
	}
}

func TestCustomRouteMethodPreservesNormalizationAndAllow(t *testing.T) {
	const method = "CUSTOM!#$%&'*+-.^_`|~0123456789"
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != method || r.URL.Path != "/custom%zz" {
			t.Errorf("upstream method=%q path=%q", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	handler, err := NewWithContext(context.Background(), &fakeStore{}, Config{Upstream: target, Workload: control.WorkloadMedia, ExecutionRoutes: []Route{{Method: "POST", Path: "/execute"}}, PassthroughRoutes: []Route{{Method: " \tcustom!#$%&'*+-.^_`|~0123456789 \n", Path: "/custom%zz"}}})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("DELETE", "/custom%25zz", nil))
	assertProxyError(t, response, http.StatusMethodNotAllowed, "route_not_allowed")
	if got := response.Header().Get("Allow"); got != method {
		t.Fatalf("Allow=%q, want %q", got, method)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(method, "/custom%25zz", nil))
	if response.Code != http.StatusNoContent || calls != 1 {
		t.Fatalf("status=%d calls=%d", response.Code, calls)
	}
}
