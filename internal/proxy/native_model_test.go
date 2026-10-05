package proxy

import (
	"context"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type nativeStore struct {
	fakeStore
	catalog control.CatalogSnapshot
}

func (s *nativeStore) Catalog(context.Context) (control.CatalogSnapshot, error) {
	return s.catalog, nil
}
func TestNativeProxyRejectsModelBeforeAdmission(t *testing.T) {
	for _, owner := range []control.Owner{control.OwnerUser, control.OwnerSupervisor} {
		t.Run(string(owner), func(t *testing.T) {
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(200) }))
			defer upstream.Close()
			u, _ := url.Parse(upstream.URL)
			s := &nativeStore{fakeStore: fakeStore{state: admittedState(owner)}, catalog: control.CatalogSnapshot{Revision: "one", Catalog: control.Catalog{Profiles: []control.Profile{{ID: "media", NativeModel: &control.NativeModel{Runtime: "ollama", Model: "selected", Endpoint: upstream.URL}}}}}}
			h, err := New(s, Config{Upstream: u, Workload: "media", ExecutionRoutes: []Route{{Method: "POST", Path: "/api/generate"}}})
			if err != nil {
				t.Fatal(err)
			}
			for _, body := range []string{`{"model":"selected","Model":"other"}`, `{"model":"selected","Keep_Alive":0}`, `{"model":"other"}`, `{"model":"selected","keep_alive":0}`, `{"model":"selected","model":"other"}`} {
				req := httptest.NewRequest("POST", "/api/generate", strings.NewReader(body))
				req.Header.Set(DefaultRequestIDHeader, "request")
				addLeaseHeaders(req, s.state.LeaseFence)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, req)
				if w.Code != 400 || calls != 0 || len(s.admitted) != 0 {
					t.Fatalf("unsafe request forwarded: %d calls=%d", w.Code, calls)
				}
			}
			s.catalog.Revision = "two"
			req := httptest.NewRequest("POST", "/api/generate", strings.NewReader(`{"model":"selected"}`))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != 409 || calls != 0 {
				t.Fatal("stale catalog forwarded")
			}
		})
	}
}
func nativeProxyFixture(t *testing.T) (*nativeStore, Config, *httptest.Server, *int) {
	t.Helper()
	calls := new(int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { *calls++; w.WriteHeader(200) }))
	t.Cleanup(server.Close)
	u, _ := url.Parse(server.URL)
	s := &nativeStore{fakeStore: fakeStore{state: admittedState(control.OwnerSupervisor)}, catalog: control.CatalogSnapshot{Revision: "one", Catalog: control.Catalog{Profiles: []control.Profile{{ID: "media", NativeModel: &control.NativeModel{Runtime: "ollama", Model: "selected", Endpoint: server.URL}}}}}}
	return s, Config{Upstream: u, Workload: "media", ExecutionRoutes: []Route{{Method: "POST", Path: "/api/generate"}}}, server, calls
}
func TestNativeProxyRoutePolicies(t *testing.T) {
	cases := map[string]func(*Config){
		"upstream":      func(c *Config) { u, _ := url.Parse("http://127.0.0.1:1"); c.Upstream = u },
		"passthrough":   func(c *Config) { c.PassthroughRoutes = []Route{{Method: "POST", Path: "/api/create"}} },
		"mutation":      func(c *Config) { c.ExecutionRoutes = []Route{{Method: "POST", Path: "/api/pull"}} },
		"method":        func(c *Config) { c.ExecutionRoutes = []Route{{Method: "GET", Path: "/api/generate"}} },
		"read mutation": func(c *Config) { c.ReadOnlyRoutes = []Route{{Method: "GET", Path: "/api/generate"}} },
		"head":          func(c *Config) { c.ReadOnlyRoutes = []Route{{Method: "HEAD", Path: "/api/ps"}} },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			s, c, _, _ := nativeProxyFixture(t)
			edit(&c)
			if _, err := New(s, c); err == nil {
				t.Fatal("unsafe routing accepted")
			}
		})
	}
	for _, family := range []string{"ollama", "llama.cpp", "vllm"} {
		s, c, _, _ := nativeProxyFixture(t)
		s.catalog.Catalog.Profiles[0].NativeModel.Runtime = family
		c.ExecutionRoutes = []Route{{Method: "POST", Path: "/v1/chat/completions"}}
		c.ReadOnlyRoutes = []Route{{Method: "GET", Path: "/v1/models"}}
		if _, err := New(s, c); err != nil {
			t.Fatal(err)
		}
		if family != "ollama" {
			c.ExecutionRoutes = []Route{{Method: "POST", Path: "/api/generate"}}
			if _, err := New(s, c); err == nil {
				t.Fatal("non-Ollama route accepted")
			}
		}
	}
	s, c, _, _ := nativeProxyFixture(t)
	for _, path := range []string{"/api/tags", "/api/ps", "/api/version"} {
		c.ReadOnlyRoutes = []Route{{Method: "GET", Path: path}}
		if _, err := New(s, c); err != nil {
			t.Fatal(err)
		}
	}
}
func TestNativeProxyValidBodyAndLimits(t *testing.T) {
	for _, owner := range []control.Owner{control.OwnerUser, control.OwnerSupervisor} {
		t.Run(string(owner), func(t *testing.T) {
			s, c, _, calls := nativeProxyFixture(t)
			s.state.Owner = owner
			h, err := New(s, c)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest("POST", "/api/generate", strings.NewReader(`{"model":"selected","prompt":"hello"}`))
			addLeaseHeaders(req, s.state.LeaseFence)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != 200 || *calls != 1 {
				t.Fatalf("valid request denied: %d", w.Code)
			}
			for _, tc := range []struct{ path, encoding, body string }{{"/api/generate?model=other", "", `{"model":"selected"}`}, {"/api/generate", "gzip", `{"model":"selected"}`}, {"/api/generate", "", strings.Repeat("x", 16<<20+1)}} {
				req := httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body))
				req.Header.Set("Content-Encoding", tc.encoding)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, req)
				if w.Code != 400 || *calls != 1 {
					t.Fatal("invalid native body forwarded")
				}
			}
		})
	}
}

type failingCatalogStore struct{ fakeStore }

func (*failingCatalogStore) Catalog(context.Context) (control.CatalogSnapshot, error) {
	return control.CatalogSnapshot{}, errors.New("catalog unavailable")
}
func TestNativeCatalogReadFailure(t *testing.T) {
	s, c, _, _ := nativeProxyFixture(t)
	if _, err := New(&failingCatalogStore{}, c); err == nil {
		t.Fatal("catalog failure ignored")
	}
	h, err := New(s, c)
	if err != nil {
		t.Fatal(err)
	}
	h.store = &failingCatalogStore{}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/api/generate", strings.NewReader(`{"model":"selected"}`)))
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
}

func (s *nativeStore) AdmitWorkTokenAtCatalog(ctx context.Context, requestID, jobID string, workload control.Workload, fence control.Fence, revision string) (string, error) {
	if revision != s.catalog.Revision {
		return "", store.ErrVersionConflict
	}
	return s.fakeStore.AdmitWorkToken(ctx, requestID, jobID, workload, fence)
}

func TestNativeAdmissionRejectsChangedCatalog(t *testing.T) {
	s, c, _, calls := nativeProxyFixture(t)
	s.state.Owner = control.OwnerSupervisor
	h, err := New(s, c)
	if err != nil {
		t.Fatal(err)
	}
	s.catalog.Revision = "changed"
	_, err = h.admitExecution(context.Background(), "new", "", s.state.LeaseFence)
	if !errors.Is(err, store.ErrVersionConflict) {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.writeWorkError(w, err)
	if w.Code != 409 || *calls != 0 || len(s.admitted) != 0 {
		t.Fatal("stale admission escaped", w.Code)
	}
}
