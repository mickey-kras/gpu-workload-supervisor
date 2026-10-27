package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestNativePolicyCatalogVersionGuard(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	profile := func(version int) control.CatalogSnapshot {
		return control.CatalogSnapshot{Revision: "one", Catalog: control.Catalog{Version: version, Profiles: []control.WorkloadProfile{{ID: "media", NativeModel: &control.NativeModel{Runtime: "vllm", Model: "selected", Endpoint: upstream.URL}}}}}
	}
	for _, version := range []int{1, 2, 4} {
		s := &nativeStore{fakeStore: fakeStore{state: admittedState(control.OwnerSupervisor)}, catalog: profile(version)}
		if _, err := NewWithContext(context.Background(), s, Config{Upstream: u, Workload: "media", ExecutionRoutes: []Route{{Method: "POST", Path: "/v1/completions"}}}); err != nil {
			t.Fatalf("catalog version %d refused: %v", version, err)
		}
	}
	for _, version := range []int{0, 3, 99} {
		s := &nativeStore{fakeStore: fakeStore{state: admittedState(control.OwnerSupervisor)}, catalog: profile(version)}
		if _, err := NewWithContext(context.Background(), s, Config{Upstream: u, Workload: "media", ExecutionRoutes: []Route{{Method: "POST", Path: "/v1/completions"}}}); err == nil {
			t.Fatalf("catalog version %d accepted", version)
		}
	}
	if !supportedCatalogVersion(1) || !supportedCatalogVersion(2) || supportedCatalogVersion(0) || supportedCatalogVersion(3) {
		t.Fatal("supportedCatalogVersion boundary wrong")
	}
}

func TestV4TrainerCatalogPreservesInferenceProxy(t *testing.T) {
	s, c, _, calls := nativeProxyFixture(t)
	s.catalog.Catalog.Version = 4
	s.catalog.Catalog.Profiles = append(s.catalog.Catalog.Profiles, control.WorkloadProfile{ID: "trainer", Trainer: &control.TrainerBinding{Version: 1}})
	h, err := NewWithContext(context.Background(), s, c)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/v1/completions", strings.NewReader(`{"model":"selected"}`))
	req.Header.Set(DefaultRequestIDHeader, "v4-inference")
	addLeaseHeaders(req, s.state.LeaseFence)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 || *calls != 1 || len(s.admitted) != 1 {
		t.Fatalf("inference broken: status=%d calls=%d admitted=%d", w.Code, *calls, len(s.admitted))
	}
}
