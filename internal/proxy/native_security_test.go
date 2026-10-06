package proxy

import (
	"context"
	"encoding/json"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestNativeCaseFoldBypass(t *testing.T) {
	for _, body := range []string{`{"model":"selected","Model":"other"}`, `{"model":"selected","Keep_Alive":0}`} {
		var got struct {
			Model     string `json:"model"`
			KeepAlive *int   `json:"keep_alive"`
		}
		calls := 0
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Error(err)
			}
			w.WriteHeader(200)
		}))
		u, _ := url.Parse(upstream.URL)
		s := &nativeStore{fakeStore: fakeStore{state: admittedState(control.OwnerSupervisor)}, catalog: control.CatalogSnapshot{Revision: "one", Catalog: control.Catalog{Profiles: []control.WorkloadProfile{{ID: "media", NativeModel: &control.NativeModel{Runtime: "ollama", Model: "selected", Endpoint: upstream.URL}}}}}}
		h, err := NewWithContext(context.Background(), s, Config{Upstream: u, Workload: "media", ExecutionRoutes: []Route{{Method: "POST", Path: "/api/generate"}}})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("POST", "/api/generate", strings.NewReader(body))
		req.Header.Set(DefaultRequestIDHeader, "review")
		addLeaseHeaders(req, s.state.LeaseFence)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		upstream.Close()
		if calls > 0 {
			t.Errorf("unsafe native request forwarded: body=%s status=%d calls=%d admitted=%d decoded_model=%q decoded_keep_alive=%v", body, w.Code, calls, len(s.admitted), got.Model, got.KeepAlive)
		}
	}
}

func TestNativeReadonlyReloadBypass(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("reload") == "1" {
			calls++
		}
		w.WriteHeader(200)
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	s := &nativeStore{fakeStore: fakeStore{state: admittedState(control.OwnerSupervisor)}, catalog: control.CatalogSnapshot{Revision: "one", Catalog: control.Catalog{Profiles: []control.WorkloadProfile{{ID: "media", NativeModel: &control.NativeModel{Runtime: "llama.cpp", Model: "selected", Endpoint: upstream.URL}}}}}}
	s.state.Admission = control.AdmissionClosed
	h, err := NewWithContext(context.Background(), s, Config{Upstream: u, Workload: "media", ExecutionRoutes: []Route{{Method: "POST", Path: "/v1/completions"}}, ReadOnlyRoutes: []Route{{Method: "GET", Path: "/v1/models"}}})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/v1/models?reload=1", nil))
	if calls > 0 {
		t.Errorf("model-list reload request forwarded with closed admission: status=%d calls=%d", w.Code, calls)
	}
}
