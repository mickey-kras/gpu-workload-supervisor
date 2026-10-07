package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestNativePolicyCatalogVersionGuard(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	profile := func(version int) control.CatalogSnapshot {
		return control.CatalogSnapshot{Revision: "one", Catalog: control.Catalog{Version: version, Profiles: []control.WorkloadProfile{{ID: "media", NativeModel: &control.NativeModel{Runtime: "ollama", Model: "selected", Endpoint: upstream.URL}}}}}
	}
	for _, version := range []int{1, 2} {
		s := &nativeStore{fakeStore: fakeStore{state: admittedState(control.OwnerSupervisor)}, catalog: profile(version)}
		if _, err := NewWithContext(context.Background(), s, Config{Upstream: u, Workload: "media", ExecutionRoutes: []Route{{Method: "POST", Path: "/api/generate"}}}); err != nil {
			t.Fatalf("catalog version %d refused: %v", version, err)
		}
	}
	for _, version := range []int{0, 3, 99} {
		s := &nativeStore{fakeStore: fakeStore{state: admittedState(control.OwnerSupervisor)}, catalog: profile(version)}
		if _, err := NewWithContext(context.Background(), s, Config{Upstream: u, Workload: "media", ExecutionRoutes: []Route{{Method: "POST", Path: "/api/generate"}}}); err == nil {
			t.Fatalf("catalog version %d accepted", version)
		}
	}
	if !supportedCatalogVersion(1) || !supportedCatalogVersion(2) || supportedCatalogVersion(0) || supportedCatalogVersion(3) {
		t.Fatal("supportedCatalogVersion boundary wrong")
	}
}
